package metrics_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	memory_storage "github.com/storage-lock/go-memory-storage"
	"github.com/storage-lock/go-remote-storage/metrics"
	"github.com/storage-lock/go-remote-storage/server"
	"github.com/storage-lock/go-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
	"github.com/stretchr/testify/require"
)

// TestRouteLabelIsLowCardinality 是最关键的一条：
// 任意 lock id 都必须归成同一个 route label，否则每个锁都会生成一条时序。
func TestRouteLabelIsLowCardinality(t *testing.T) {
	paths := []string{
		"/v1/locks/order-42",
		"/v1/locks/user-123",
		"/v1/locks/a/b/c",
		"/v1/locks/aaaaaaaaaaaaaaaaaaaa",
	}
	for _, p := range paths {
		require.Equal(t, "/v1/locks/{id}", metrics.RouteLabel(p), "path=%s", p)
	}

	// 扫描器用随机路径：必须都归到 other，不能每个都产生新 label。
	for _, p := range []string{"/wp-admin", "/.env", "/random-1", "/random-2"} {
		require.Equal(t, "other", metrics.RouteLabel(p))
	}
}

// TestMetricsEndpointServes 验证 /metrics 端点可访问且输出合法格式。
func TestMetricsEndpointServes(t *testing.T) {
	reg := metrics.New("remote_storage")
	reg.SetVersion("test")

	h, err := server.New(server.Options{
		Storage: memory_storage.NewMemoryStorage(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: reg,
	})
	require.NoError(t, err)
	srv := httptest.NewServer(metrics.HTTPMiddleware(reg, h.Routes()))
	defer srv.Close()

	// 先打一次正常请求，让 HTTP 计数器有数据（CounterVec 未被调用时不出现）。
	_, err = http.Get(srv.URL + "/v1/health")
	require.NoError(t, err)

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	out := string(body)
	require.Contains(t, out, "remote_storage_build_info")
	require.Contains(t, out, "remote_storage_http_requests_total")
	// Prometheus 文本格式以 # HELP / # TYPE 开头
	require.Contains(t, out, "# HELP")
	require.Contains(t, out, "# TYPE")
}

// TestStorageWrapperDoesNotAlterSemantics 验证指标包装器不改变错误语义。
func TestStorageWrapperDoesNotAlterSemantics(t *testing.T) {
	reg := metrics.New("test")
	inner := memory_storage.NewMemoryStorage()
	wrapped := metrics.WrapStorage(inner, reg)
	ctx := context.Background()

	// 不存在 → ErrLockNotFound，必须原样传出。
	_, err := wrapped.Get(ctx, "absent")
	require.ErrorIs(t, err, storage_lock.ErrLockNotFound,
		"包装器不能把错误换成别的")

	// 版本冲突 → ErrVersionMiss，必须原样传出（上层靠它决定重试）。
	require.NoError(t, wrapped.CreateWithVersion(ctx, "k", 1,
		&storage.LockInformation{LockId: "k", OwnerId: "o", Version: 1}))
	err = wrapped.UpdateWithVersion(ctx, "k", 99, 100,
		&storage.LockInformation{LockId: "k", OwnerId: "o", Version: 100})
	require.ErrorIs(t, err, storage_lock.ErrVersionMiss)
}

// TestStorageWrapperRecordsOperations 验证每个操作都被计数。
func TestStorageWrapperRecordsOperations(t *testing.T) {
	reg := metrics.New("test")
	wrapped := metrics.WrapStorage(memory_storage.NewMemoryStorage(), reg)
	ctx := context.Background()

	require.NoError(t, wrapped.CreateWithVersion(ctx, "k", 1,
		&storage.LockInformation{LockId: "k", OwnerId: "o", Version: 1}))
	_, _ = wrapped.Get(ctx, "k")
	_, _ = wrapped.GetTime(ctx)
	_, _ = wrapped.List(ctx)

	// 通过 /metrics 输出验证计数器出现。
	h, _ := server.New(server.Options{
		Storage: wrapped, Metrics: reg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)
	require.Contains(t, out, `operation="create"`)
	require.Contains(t, out, `operation="get"`)
	require.Contains(t, out, `operation="get_time"`)
}

// TestAuthFailureCounted 验证鉴权失败有独立计数器，便于告警。
func TestAuthFailureCounted(t *testing.T) {
	reg := metrics.New("test")
	h, err := server.New(server.Options{
		Storage:       memory_storage.NewMemoryStorage(),
		Authenticator: server.BearerAuth{Token: "correct"},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:       reg,
	})
	require.NoError(t, err)
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 401, resp.StatusCode)

	// 读指标确认计数为 1。
	mresp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer mresp.Body.Close()
	body, _ := io.ReadAll(mresp.Body)
	require.Contains(t, string(body), "test_auth_failures_total 1",
		"鉴权失败必须计数，0123 否则无法配告警")
}


// TestRouteLabelDistinguishesServices 验证两个服务的同名路径不会互相混淆。
// 早期版本用子串匹配，" /v1/locks " 会命中 "/api/v1/locks"；
// 现在改成精确前缀匹配，从最具体到最宽泛。
func TestRouteLabelDistinguishesServices(t *testing.T) {
	cases := map[string]string{
		"/v1/locks":            "/v1/locks",
		"/v1/locks/x":          "/v1/locks/{id}",
		"/api/v1/locks":        "/api/v1/locks",
		"/api/v1/locks/x":      "/api/v1/locks/{id}",
		"/api/v1/tenant":       "/api/v1/tenant",
		"/api/v1/tenants":      "/api/v1/tenant",
		"/api/v1/endpoints":    "/api/v1/endpoints",
		"/api/v1/endpoints/best": "/api/v1/endpoints",
		"/health":              "/health",
		"/v1/health":           "/health",
		"/metrics":             "/metrics",
	}
	for path, want := range cases {
		require.Equal(t, want, metrics.RouteLabel(path), "path=%s", path)
	}
}
