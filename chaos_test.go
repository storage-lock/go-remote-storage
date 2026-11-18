package remote_storage_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	memory_storage "github.com/storage-lock/go-memory-storage"
	"github.com/storage-lock/go-remote-storage/client"
	"github.com/storage-lock/go-remote-storage/server"
	"github.com/storage-lock/go-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
	"github.com/stretchr/testify/require"
)

// 混沌测试：在真实 HTTP 链路上注入故障，验证客户端不会因此产生错误的"成功"判断。
//
// 分布式锁最危险的失效不是"报错"，而是"明明没成功却报告成功"——
// 后者会让两个进程同时进入临界区。
//
// 三类注入：
//   - dropResponse：服务端处理了，但响应丢掉 → 客户端应看到错误
//   - slowResponse：响应很慢 → 客户端应按自己的 ctx 超时
//   - failRequest：请求根本没到服务端 → 客户端应看到错误

type chaosProxy struct {
	backend  *url.URL
	srv      *httptest.Server
	dropRate int32 // 每 N 个请求丢一个响应，0 表示不丢
	sleep    time.Duration
	count    int32
}

func newChaosProxy(t *testing.T, backendURL string) *chaosProxy {
	t.Helper()
	u, err := url.Parse(backendURL)
	require.NoError(t, err)
	p := &chaosProxy{backend: u}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *chaosProxy) handle(w http.ResponseWriter, r *http.Request) {
	n := atomic.AddInt32(&p.count, 1)

	if p.dropRate > 0 && int(n)%int(p.dropRate) == 0 {
		// 把请求完整转发给后端（让它真的执行），然后丢弃响应、断开连接。
		rp := httputil.NewSingleHostReverseProxy(p.backend)
		rec := httptest.NewRecorder()
		rp.ServeHTTP(rec, r)
		// 断开连接：客户端会看到 unexpected EOF，而不是一个响应。
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close() // 直接关，不写任何东西
				return
			}
		}
		return
	}

	if p.sleep > 0 {
		select {
		case <-time.After(p.sleep):
		case <-r.Context().Done():
			return
		}
	}

	rp := httputil.NewSingleHostReverseProxy(p.backend)
	rp.ServeHTTP(w, r)
}

func startRealServer(t *testing.T) *httptest.Server {
	t.Helper()
	h, err := server.New(server.Options{
		Storage: memory_storage.NewMemoryStorage(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv
}

// TestUpdateTimeoutDoesNotReportSuccess 是核心安全断言：
// 当响应丢失、客户端超时时，Update 绝不能返回 nil。
// 如果它返回 nil，上层会以为 CAS 成功，可能重复执行受保护的操作。
func TestUpdateTimeoutDoesNotReportSuccess(t *testing.T) {
	srv := startRealServer(t)
	proxy := newChaosProxy(t, srv.URL)

	rs, err := client.New(context.Background(), client.Config{
		BaseURL: proxy.srv.URL,
		Timeout: 300 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx := context.Background()
	info := &storage.LockInformation{LockId: "chaos", OwnerId: "o", Version: 1}
	require.NoError(t, rs.CreateWithVersion(ctx, "chaos", 1, info))

	// 第 2 个请求（即这次 Update）丢响应。
	atomic.StoreInt32(&proxy.dropRate, 2)
	defer atomic.StoreInt32(&proxy.dropRate, 0)
	atomic.StoreInt32(&proxy.count, 1) // 让下一个请求成为"第 2 个"

	info.Version = 2
	err = rs.UpdateWithVersion(ctx, "chaos", 1, 2, info)
	require.Error(t, err, "响应丢失时必须报错，绝不能返回 nil")
	t.Logf("客户端看到: %v", err)
}

// TestCreateTimeoutDoesNotReportSuccess 同上，针对 Create。
func TestCreateTimeoutDoesNotReportSuccess(t *testing.T) {
	srv := startRealServer(t)
	proxy := newChaosProxy(t, srv.URL)

	rs, err := client.New(context.Background(), client.Config{
		BaseURL: proxy.srv.URL,
		Timeout: 300 * time.Millisecond,
	})
	require.NoError(t, err)

	atomic.StoreInt32(&proxy.dropRate, 1) // 第一个请求就丢
	err = rs.CreateWithVersion(context.Background(), "c", 1,
		&storage.LockInformation{LockId: "c", OwnerId: "o", Version: 1})
	require.Error(t, err, "响应丢失时必须报错")
	t.Logf("客户端看到: %v", err)
	atomic.StoreInt32(&proxy.dropRate, 0)
}

// TestSlowServerRespectsClientDeadline 验证客户端 ctx 到期时不会无限挂住。
func TestSlowServerRespectsClientDeadline(t *testing.T) {
	srv := startRealServer(t)
	proxy := newChaosProxy(t, srv.URL)
	proxy.sleep = 5 * time.Second

	rs, err := client.New(context.Background(), client.Config{
		BaseURL: proxy.srv.URL,
		Timeout: 10 * time.Second,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = rs.GetTime(ctx)
	elapsed := time.Since(start)

	require.Error(t, err, "客户端 ctx 到期必须返回错误")
	require.Less(t, elapsed, 2*time.Second,
		"必须在 ctx 到期附近返回，而不是等满 5 秒服务端延迟")
	t.Logf("耗时 %v，错误 %v", elapsed, err)
}

// TestConcurrentUpdatesOnlyOneWins 模拟真实争抢：同一版本上并发 Update，
// 只能有一个成功，其余必须是 ErrVersionMiss。
func TestConcurrentUpdatesOnlyOneWins(t *testing.T) {
	srv := startRealServer(t)
	rs, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, rs.CreateWithVersion(ctx, "race", 1,
		&storage.LockInformation{LockId: "race", OwnerId: "init", Version: 1}))

	const n = 16
	var wins, misses int32
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			err := rs.UpdateWithVersion(ctx, "race", 1, 2,
				&storage.LockInformation{LockId: "race", OwnerId: fmt.Sprintf("w%d", i), Version: 2})
			switch {
			case err == nil:
				atomic.AddInt32(&wins, 1)
			case isVersionMiss(err):
				atomic.AddInt32(&misses, 1)
			}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}

	require.Equal(t, int32(1), atomic.LoadInt32(&wins),
		"同一版本上只能有一个更新成功")
	require.Equal(t, int32(n-1), atomic.LoadInt32(&misses))
}

func isVersionMiss(err error) bool {
	return errors.Is(err, storage_lock.ErrVersionMiss)
}
