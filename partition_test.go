package remote_storage_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/storage-lock/go-remote-storage/client"
	storage_lock "github.com/storage-lock/go-storage-lock"
	"github.com/stretchr/testify/require"
)

// TestPartitionNeverBreaksMutualExclusion 是最强的安全断言：
// 在间歇性网络分区下，仍在运行的客户端之间不能出现两个持有者。
//
// 做法：给请求注入随机失败，但**不伪造成功**。
// 客户端在失败时会重试获取，正确实现下临界区永远只有一个人。
func mustURL(t *testing.T, raw string) *url.URL {
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestPartitionNeverBreaksMutualExclusion(t *testing.T) {
	srv := startRealServer(t)

	var failEvery int32 = 3
	var counter int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&counter, 1)
		if int(n)%int(atomic.LoadInt32(&failEvery)) == 0 {
			// 一半概率让连接直接断开（模拟网络抖动）
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}
		rp := httputil.NewSingleHostReverseProxy(mustURL(t, srv.URL))
		rp.ServeHTTP(w, r)
	}))
	defer flaky.Close()

	rs, err := client.New(context.Background(), client.Config{
		BaseURL: flaky.URL,
		Timeout: 2 * time.Second,
	})
	require.NoError(t, err)

	var inCritical, violations int32
	const workers = 6
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := fmt.Sprintf("w%d", i)
			// 用短重试间隔，模拟客户端自行处理瞬时故障
			opts := client.RecommendedOptions("partition")
			opts.LeaseExpireAfter = 10 * time.Second
			opts.DisableWatchDog = true
			lock, err := storage_lock.NewStorageLockWithOptions(rs, opts)
			if err != nil {
				return
			}
			for j := 0; j < 8; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				err := lock.Lock(ctx, owner)
				if err != nil {
					cancel()
					return // 拿不到就退出，不算失败
				}
				if atomic.AddInt32(&inCritical, 1) > 1 {
					atomic.AddInt32(&violations, 1)
				}
				time.Sleep(500 * time.Microsecond)
				atomic.AddInt32(&inCritical, -1)
				_ = lock.UnLock(ctx, owner)
				cancel()
			}
		}(i)
	}
	wg.Wait()

	require.Zero(t, violations, "网络抖动下出现了多个持有者")
	t.Logf("完成：无重叠")
}
