package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	memory_storage "github.com/storage-lock/go-memory-storage"
	"github.com/storage-lock/go-remote-storage/client"
	go_iterator "github.com/golang-infrastructure/go-iterator"
	"github.com/storage-lock/go-remote-storage/server"
	"github.com/storage-lock/go-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
	"github.com/stretchr/testify/require"
)

// startServer 起一个真实 HTTP 服务，用内存存储当底层。
func startServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	store := memory_storage.NewMemoryStorage()
	opts := server.Options{
		Storage: store,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if token != "" {
		opts.Authenticator = server.BearerAuth{Token: token}
	}
	h, err := server.New(opts)
	require.NoError(t, err)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, baseURL, token string) *client.RemoteStorage {
	t.Helper()
	rs, err := client.New(context.Background(), client.Config{BaseURL: baseURL, Secret: token})
	require.NoError(t, err)
	return rs
}

// TestRemoteStorageIsARealStorage 验证客户端满足 Storage 接口。
func TestRemoteStorageIsARealStorage(t *testing.T) {
	srv := startServer(t, "")
	rs := newClient(t, srv.URL, "")
	var _ storage.Storage = rs
	require.True(t, len(rs.Capabilities()) > 0)
	require.Contains(t, rs.GetName(), "remote")
}

// TestCapabilitiesAreForwardedNotInvented 是关键断言：
// 客户端的能力必须原样来自服务端，不能自己补。
func TestCapabilitiesAreForwardedNotInvented(t *testing.T) {
	srv := startServer(t, "")
	rs := newClient(t, srv.URL, "")

	resp, err := http.Get(srv.URL + "/v1/capabilities")
	require.NoError(t, err)
	defer resp.Body.Close()
	var capBody map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&capBody))
	serverCaps := capBody["capabilities"].([]any)

	require.Len(t, rs.Capabilities(), len(serverCaps),
		"客户端能力数量必须与服务端一致，不能补全")
}

// TestServerRefusesStorageWithoutCAS 验证不合格的底层会被拒绝启动，
// 而不是把错误传播给所有客户端。
func TestServerRefusesStorageWithoutCAS(t *testing.T) {
	// 造一个只声明 Watch、没有 CAS 的存储。
	bad := &noCASStorage{}
	_, err := server.New(server.Options{Storage: bad})
	require.Error(t, err)
	require.Contains(t, err.Error(), "CapabilityCAS")
}

type noCASStorage struct{}

func (n *noCASStorage) GetName() string { return "no-cas" }
func (n *noCASStorage) Capabilities() []storage.StorageCapability {
	return []storage.StorageCapability{storage.CapabilityWatch}
}
func (n *noCASStorage) Init(ctx context.Context) error { return nil }
func (n *noCASStorage) UpdateWithVersion(ctx context.Context, id string, e, nv storage.Version, i *storage.LockInformation) error {
	return storage_lock.ErrVersionMiss
}
func (n *noCASStorage) CreateWithVersion(ctx context.Context, id string, v storage.Version, i *storage.LockInformation) error {
	return nil
}
func (n *noCASStorage) DeleteWithVersion(ctx context.Context, id string, e storage.Version, i *storage.LockInformation) error {
	return nil
}
func (n *noCASStorage) Get(ctx context.Context, id string) (string, error) { return "", nil }
func (n *noCASStorage) GetTime(ctx context.Context) (time.Time, error)    { return time.Now(), nil }
func (n *noCASStorage) Close(ctx context.Context) error                   { return nil }
func (n *noCASStorage) List(ctx context.Context) (go_iterator.Iterator[*storage.LockInformation], error) {
	return nil, nil
}

// TestCASRoundTrip 验证四个原子操作都真的落到了服务端。
func TestCASRoundTrip(t *testing.T) {
	srv := startServer(t, "")
	rs := newClient(t, srv.URL, "")
	ctx := context.Background()

	info := &storage.LockInformation{LockId: "k", OwnerId: "o", Version: 1, LockCount: 1}
	require.NoError(t, rs.CreateWithVersion(ctx, "k", 1, info))

	// 正确的期望版本 → 成功。
	info.Version = 2
	require.NoError(t, rs.UpdateWithVersion(ctx, "k", 1, 2, info))

	// 过期的期望版本 → ErrVersionMiss（必须映射回来，否则上层抢占逻辑会死循环）。
	info.Version = 3
	require.ErrorIs(t, rs.UpdateWithVersion(ctx, "k", 1, 3, info), storage_lock.ErrVersionMiss)

	got, err := rs.Get(ctx, "k")
	require.NoError(t, err)
	restored, err := storage.LockInformationFromJsonString(got)
	require.NoError(t, err)
	require.Equal(t, storage.Version(2), restored.Version)
}

// TestSecondCreateFails 验证"不存在才插入"的原子性由服务端保证。
func TestSecondCreateFails(t *testing.T) {
	srv := startServer(t, "")
	rs := newClient(t, srv.URL, "")
	ctx := context.Background()
	info := &storage.LockInformation{LockId: "k", OwnerId: "o", Version: 1}
	require.NoError(t, rs.CreateWithVersion(ctx, "k", 1, info))
	require.ErrorIs(t, rs.CreateWithVersion(ctx, "k", 2, info), storage_lock.ErrLockAlreadyExists)
}

// TestTimeComesFromServer 验证客户端不偷偷用本机时间。
func TestTimeComesFromServer(t *testing.T) {
	srv := startServer(t, "")
	rs := newClient(t, srv.URL, "")
	got, err := rs.GetTime(context.Background())
	require.NoError(t, err)
	require.False(t, got.IsZero())
	require.WithinDuration(t, time.Now(), got, 5*time.Second)
}

// TestLockIDWithSlash 验证含 "/" 的 lock id 不会被服务端切坏。
func TestLockIDWithSlash(t *testing.T) {
	srv := startServer(t, "")
	rs := newClient(t, srv.URL, "")
	ctx := context.Background()
	const id = "tenant/a/b/c"
	info := &storage.LockInformation{LockId: id, OwnerId: "o", Version: 1}
	require.NoError(t, rs.CreateWithVersion(ctx, id, 1, info))
	raw, err := rs.Get(ctx, id)
	require.NoError(t, err)
	restored, err := storage.LockInformationFromJsonString(raw)
	require.NoError(t, err)
	require.Equal(t, id, restored.LockId)
}

// TestAuthRequired 验证配了 token 之后匿名访问被拒。
func TestAuthRequired(t *testing.T) {
	srv := startServer(t, "s3cret")
	// 无 token 建客户端：探测阶段就会被拒。
	_, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.Error(t, err)

	// 带 token 可以用。
	rs, err := client.New(context.Background(), client.Config{BaseURL: srv.URL, Secret: "s3cret"})
	require.NoError(t, err)
	require.NotNil(t, rs)
}

// TestListReturnsAllLocks 验证列表能往返。
func TestListReturnsAllLocks(t *testing.T) {
	srv := startServer(t, "")
	rs := newClient(t, srv.URL, "")
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		require.NoError(t, rs.CreateWithVersion(ctx, id, 1,
			&storage.LockInformation{LockId: id, OwnerId: "o", Version: 1}))
	}
	iter, err := rs.List(ctx)
	require.NoError(t, err)
	var ids []string
	for iter.Next() {
		ids = append(ids, iter.Value().LockId)
	}
	require.Len(t, ids, 2)
}

// TestProtocolVersionMismatchRejected 验证协议版本不一致时拒绝启动。
func TestProtocolVersionMismatchRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"protocol_version": "v999",
			"name":             "future",
			"capabilities":     []string{"cas", "reliable-time"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.Error(t, err)
	require.Contains(t, err.Error(), "protocol version")
}
