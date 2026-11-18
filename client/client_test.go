package client_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	memory_storage "github.com/storage-lock/go-memory-storage"
	"github.com/storage-lock/go-remote-storage/client"
	"github.com/storage-lock/go-remote-storage/server"
	"github.com/storage-lock/go-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
	"github.com/stretchr/testify/require"
)

func realServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	opts := server.Options{
		Storage: memory_storage.NewMemoryStorage(),
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

// TestConnectRefusesWhenUnreachable 验证连不上时拒绝构造，
// 而不是返回一个"能用但每次调用都失败"的客户端。
func TestConnectRefusesWhenUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	_, err := client.New(context.Background(), client.Config{BaseURL: url})
	require.Error(t, err)
	require.Contains(t, err.Error(), "probe")
}

// TestConnectRefusesWrongToken 验证凭据错误在构造阶段就暴露。
func TestConnectRefusesWrongToken(t *testing.T) {
	srv := realServer(t, "correct")
	_, err := client.New(context.Background(), client.Config{BaseURL: srv.URL, Secret: "wrong"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthorized")
}

// TestConnectRefusesWithoutToken 验证服务端要求凭据而客户端没给时拒绝。
func TestConnectRefusesWithoutToken(t *testing.T) {
	srv := realServer(t, "correct")
	_, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.Error(t, err)
}

// TestNonJSONEndpointGivesActionableError 验证地址指错服务时给出可行动的提示。
func TestNonJSONEndpointGivesActionableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>some other service</html>"))
	}))
	defer srv.Close()

	_, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-JSON")
	require.Contains(t, err.Error(), "is the base URL")
}

// TestBaseURLRequired 验证空地址直接报错。
func TestBaseURLRequired(t *testing.T) {
	_, err := client.New(context.Background(), client.Config{})
	require.Error(t, err)
}

// TestSchemeRejected 验证非 http(s) 地址被拒。
func TestSchemeRejected(t *testing.T) {
	_, err := client.New(context.Background(), client.Config{BaseURL: "ftp://example.com"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "http(s)")
}

// TestNoAutoRetryOnVersionMiss 验证客户端不自己重试版本冲突——
// 重试策略必须由上层决定，否则抢占路径会被掩盖。
func TestNoAutoRetryOnVersionMiss(t *testing.T) {
	srv := realServer(t, "")
	rs, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, rs.CreateWithVersion(ctx, "k", 5,
		&storage.LockInformation{LockId: "k", OwnerId: "o", Version: 5}))

	start := time.Now()
	err = rs.UpdateWithVersion(ctx, "k", 1, 2,
		&storage.LockInformation{LockId: "k", OwnerId: "o2", Version: 2})
	require.ErrorIs(t, err, storage_lock.ErrVersionMiss)
	require.Less(t, time.Since(start), 2*time.Second, "不应内部重试")
}

// TestTimeDoesNotFallBackToLocalClock 验证时间只来自服务端。
func TestTimeDoesNotFallBackToLocalClock(t *testing.T) {
	srv := realServer(t, "")
	rs, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.NoError(t, err)

	// 把 /v1/time 换成返回固定时间的 mock，确认客户端采纳的是服务端值。
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"protocol_version":"v1","name":"mock","capabilities":["cas","reliable-time"]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"unix_nano":1234567890123456789}`))
	}))
	defer mock.Close()

	rs2, err := client.New(context.Background(), client.Config{BaseURL: mock.URL})
	require.NoError(t, err)
	got, err := rs2.GetTime(context.Background())
	require.NoError(t, err)
	require.Equal(t, time.Unix(0, 1234567890123456789).UTC(), got,
		"必须使用服务端返回的时间，不能回退本机时钟")
	_ = rs
}
