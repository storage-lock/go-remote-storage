package client_test

import (
	"context"
	"testing"

	"github.com/storage-lock/go-remote-storage/client"
	"github.com/storage-lock/go-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
	"github.com/stretchr/testify/require"
)

// TestAllStorageMethods 逐个覆盖 Storage 接口的每个方法，
// 确保没有一条路径是"写了但没人验证过"。
func TestAllStorageMethods(t *testing.T) {
	srv := realServer(t, "")
	rs, err := newClient2(t, srv.URL)
	require.NoError(t, err)
	ctx := context.Background()

	// GetName / Capabilities
	require.NotEmpty(t, rs.GetName())
	require.NotEmpty(t, rs.Capabilities())

	// Init 可用（重新探活）
	require.NoError(t, rs.Init(ctx))

	// GetTime
	tm, err := rs.GetTime(ctx)
	require.NoError(t, err)
	require.False(t, tm.IsZero())

	// Create → Get → Update → Delete 全链路
	info := &storage.LockInformation{LockId: "k", OwnerId: "o", Version: 1, LockCount: 1}
	require.NoError(t, rs.CreateWithVersion(ctx, "k", 1, info))

	raw, err := rs.Get(ctx, "k")
	require.NoError(t, err)
	restored, err := storage.LockInformationFromJsonString(raw)
	require.NoError(t, err)
	require.Equal(t, storage.Version(1), restored.Version)

	info.Version = 2
	require.NoError(t, rs.UpdateWithVersion(ctx, "k", 1, 2, info))

	require.NoError(t, rs.DeleteWithVersion(ctx, "k", 2, info))
	_, err = rs.Get(ctx, "k")
	require.ErrorIs(t, err, storage_lock.ErrLockNotFound)

	// List（此时已删空）
	iter, err := rs.List(ctx)
	require.NoError(t, err)
	require.False(t, iter.Next())

	// Close
	require.NoError(t, rs.Close(ctx))
}

// TestGetMissingReturnsLockNotFound 验证不存在的锁映射正确。
func TestGetMissingReturnsLockNotFound(t *testing.T) {
	srv := realServer(t, "")
	rs, err := newClient2(t, srv.URL)
	require.NoError(t, err)
	_, err = rs.Get(context.Background(), "absent")
	require.ErrorIs(t, err, storage_lock.ErrLockNotFound)
}

// TestListReturnsParsedInformation 验证 List 解析出的是完整结构。
func TestListReturnsParsedInformation(t *testing.T) {
	srv := realServer(t, "")
	rs, err := newClient2(t, srv.URL)
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, rs.CreateWithVersion(ctx, "a", 1,
		&storage.LockInformation{LockId: "a", OwnerId: "owner-a", Version: 1, LockCount: 1}))

	iter, err := rs.List(ctx)
	require.NoError(t, err)
	var seen *storage.LockInformation
	for iter.Next() {
		seen = iter.Value()
	}
	require.NotNil(t, seen)
	require.Equal(t, "a", seen.LockId)
	require.Equal(t, "owner-a", seen.OwnerId)
	require.Equal(t, storage.Version(1), seen.Version)
}

// TestInitReprobes 验证 Init 会重新探测，服务端挂了 Init 会失败。
func TestInitReprobes(t *testing.T) {
	srv := realServer(t, "")
	rs, err := newClient2(t, srv.URL)
	require.NoError(t, err)
	require.NoError(t, rs.Init(context.Background()))

	srv.Close()
	require.Error(t, rs.Init(context.Background()), "服务端关闭后 Init 必须失败")
}

func newClient2(t *testing.T, baseURL string) (*client.RemoteStorage, error) {
	t.Helper()
	return client.New(context.Background(), client.Config{BaseURL: baseURL})
}
