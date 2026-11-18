package remote_storage_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	memory_storage "github.com/storage-lock/go-memory-storage"
	"github.com/storage-lock/go-remote-storage/client"
	"github.com/storage-lock/go-remote-storage/server"
	storage_lock "github.com/storage-lock/go-storage-lock"
	"github.com/stretchr/testify/require"
)

// 这些测试是整套设计是否成立的关键：
// 远程存储必须真的能承载分布式锁的全部语义，而不只是"能读写 JSON"。

func newRemoteStore(t *testing.T) *client.RemoteStorage {
	t.Helper()
	h, err := server.New(server.Options{
		Storage: memory_storage.NewMemoryStorage(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)

	rs, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.NoError(t, err)
	return rs
}

// TestRemoteStoreBacksRealLock 验证最基础的互斥。
func TestRemoteStoreBacksRealLock(t *testing.T) {
	ctx := context.Background()
	store := newRemoteStore(t)

	a, err := storage_lock.NewStorageLock(store, "demo")
	require.NoError(t, err)
	b, err := storage_lock.NewStorageLock(store, "demo")
	require.NoError(t, err)

	require.NoError(t, a.Lock(ctx, "owner-a"))
	require.ErrorIs(t, b.TryLock(ctx, "owner-b"), storage_lock.ErrLockBusy)
	require.NoError(t, a.UnLock(ctx, "owner-a"))
	require.NoError(t, b.TryLock(ctx, "owner-b"))
}

// TestRemoteStoreMutualExclusionUnderConcurrency 是核心断言：
// 多个客户端同时抢锁，临界区不能重叠。
func TestRemoteStoreMutualExclusionUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	store := newRemoteStore(t)

	var inCritical, violations int32
	const goroutines = 8
	const rounds = 10
	done := make(chan struct{}, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			lock, err := storage_lock.NewStorageLock(store, "hot")
			if err != nil {
				return
			}
			owner := fmt.Sprintf("worker-%d", i)
			for j := 0; j < rounds; j++ {
				opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := lock.Lock(opCtx, owner); err != nil {
					cancel()
					return
				}
				if atomic.AddInt32(&inCritical, 1) > 1 {
					atomic.AddInt32(&violations, 1)
				}
				time.Sleep(200 * time.Microsecond)
				atomic.AddInt32(&inCritical, -1)
				_ = lock.UnLock(opCtx, owner)
				cancel()
			}
		}(i)
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	require.Zero(t, violations, "远程存储上的互斥被破坏")
}

// TestRemoteStoreFairLockMutualExclusion 覆盖 v0.0.7 那个 P0 漏洞的场景：
// 两个独立的 FairLock 实例必须互斥。
func TestRemoteStoreFairLockMutualExclusion(t *testing.T) {
	ctx := context.Background()
	store := newRemoteStore(t)

	a, err := storage_lock.NewFairLock(store, "fair")
	require.NoError(t, err)
	b, err := storage_lock.NewFairLock(store, "fair")
	require.NoError(t, err)

	require.NoError(t, a.Lock(ctx, "owner-a"))
	require.Error(t, b.TryLock(ctx, "owner-b"), "第二个客户端不能拿到已被占用的公平锁")
	require.NoError(t, a.UnLock(ctx, "owner-a"))
	require.NoError(t, b.TryLock(ctx, "owner-b"))
}

// TestRemoteStoreLeaseExpiry 验证租约过期后可以被抢占。
func TestRemoteStoreLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	store := newRemoteStore(t)

	opts := storage_lock.NewStorageLockOptionsWithLockId("lease")
	opts.DisableWatchDog = true
	opts.LeaseExpireAfter = 4 * time.Second
	opts.VersionMissRetryInterval = 50 * time.Millisecond

	a, err := storage_lock.NewStorageLockWithOptions(store, opts)
	require.NoError(t, err)
	require.NoError(t, a.Lock(ctx, "owner-a"))

	// 立刻抢应该失败。
	b, err := storage_lock.NewStorageLockWithOptions(store, opts)
	require.NoError(t, err)
	require.ErrorIs(t, b.TryLock(ctx, "owner-b"), storage_lock.ErrLockBusy)

	// 等租约过期（4s）后再抢。
	time.Sleep(4500 * time.Millisecond)
	okCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, b.Lock(okCtx, "owner-b"), "租约过期后应可被抢占")
}

// TestRemoteStoreFencingTokenMonotonic 验证 fencing token 只增不减。
func TestRemoteStoreFencingTokenMonotonic(t *testing.T) {
	ctx := context.Background()
	store := newRemoteStore(t)

	lock, err := storage_lock.NewStorageLock(store, "fence")
	require.NoError(t, err)

	prevToken := uint64(0)
	for i := 0; i < 5; i++ {
		owner := fmt.Sprintf("owner-%d", i)
		require.NoError(t, lock.Lock(ctx, owner))
		status, err := lock.Inspect(ctx)
		require.NoError(t, err)
		tokenNow := uint64(status.Information.Version)
		require.Greater(t, tokenNow, prevToken, "fencing token 必须单调递增")
		prevToken = tokenNow
		require.NoError(t, lock.UnLock(ctx, owner))
	}
}
