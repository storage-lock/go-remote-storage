package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	memory_storage "github.com/storage-lock/go-memory-storage"
	"github.com/storage-lock/go-remote-storage/server"
	go_iterator "github.com/golang-infrastructure/go-iterator"
	"github.com/storage-lock/go-storage"
	"github.com/stretchr/testify/require"
)

var errDown = errors.New("storage down")

// failingTimeStorage 正常声明能力，但 GetTime 会失败。
// 用来验证健康检查真的打存储。
type failingTimeStorage struct {
	inner storage.Storage
	down  bool
}

func (f *failingTimeStorage) GetName() string { return f.inner.GetName() }
func (f *failingTimeStorage) Capabilities() []storage.StorageCapability {
	return f.inner.Capabilities()
}
func (f *failingTimeStorage) Init(ctx context.Context) error { return nil }
func (f *failingTimeStorage) Close(ctx context.Context) error { return nil }
func (f *failingTimeStorage) CreateWithVersion(ctx context.Context, id string, v storage.Version, i *storage.LockInformation) error {
	return f.inner.CreateWithVersion(ctx, id, v, i)
}
func (f *failingTimeStorage) UpdateWithVersion(ctx context.Context, id string, e, nv storage.Version, i *storage.LockInformation) error {
	return f.inner.UpdateWithVersion(ctx, id, e, nv, i)
}
func (f *failingTimeStorage) DeleteWithVersion(ctx context.Context, id string, e storage.Version, i *storage.LockInformation) error {
	return f.inner.DeleteWithVersion(ctx, id, e, i)
}
func (f *failingTimeStorage) Get(ctx context.Context, id string) (string, error) {
	return f.inner.Get(ctx, id)
}
func (f *failingTimeStorage) GetTime(ctx context.Context) (time.Time, error) {
	if f.down {
		return time.Time{}, errDown
	}
	return f.inner.GetTime(ctx)
}
func (f *failingTimeStorage) List(ctx context.Context) (go_iterator.Iterator[*storage.LockInformation], error) {
	return f.inner.List(ctx)
}

// TestHealthFailsWhenStorageDown 验证底层坏了时健康检查返回 503，
// 而不是无条件回 ok（那会让监控失去意义）。
func TestHealthFailsWhenStorageDown(t *testing.T) {
	f := &failingTimeStorage{inner: memory_storage.NewMemoryStorage()}
	h, err := server.New(server.Options{
		Storage: f, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	f.down = true
	resp2, err := http.Get(srv.URL + "/v1/health")
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp2.StatusCode,
		"底层不可用时必须报 503")
}
