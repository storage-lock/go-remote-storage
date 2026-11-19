package logx_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/storage-lock/go-remote-storage/logx"
	"github.com/stretchr/testify/require"
)

// syncBuffer 是并发安全的 buffer：后台 reporter goroutine 会同时写。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogger(level slog.Level) (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level})), buf
}

// TestRequestIDIsInjected 验证每个请求都有 request_id，并回写给客户端。
func TestRequestIDIsInjected(t *testing.T) {
	logger, _ := captureLogger(slog.LevelDebug)
	var gotID string
	h := logx.Middleware(logx.MiddlewareOptions{Logger: logger})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotID = logx.RequestIDFrom(r.Context())
			w.WriteHeader(200)
		}))

	req := httptest.NewRequest("GET", "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.NotEmpty(t, gotID)
	require.Equal(t, gotID, rec.Header().Get("X-Request-Id"))
}

// TestClientRequestIDRespected 验证客户端传来的 ID 会被沿用（便于跨服务追踪）。
func TestClientRequestIDRespected(t *testing.T) {
	logger, _ := captureLogger(slog.LevelDebug)
	var gotID string
	h := logx.Middleware(logx.MiddlewareOptions{Logger: logger})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotID = logx.RequestIDFrom(r.Context())
		}))

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-Id", "client-provided-id")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, "client-provided-id", gotID)
}

// TestOversizedRequestIDReplaced 验证超长 ID 被替换，防止日志注入/内存放大。
func TestOversizedRequestIDReplaced(t *testing.T) {
	logger, _ := captureLogger(slog.LevelDebug)
	var gotID string
	h := logx.Middleware(logx.MiddlewareOptions{Logger: logger})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotID = logx.RequestIDFrom(r.Context())
		}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-Id", strings.Repeat("a", 200))
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.Less(t, len(gotID), 64)
}

// TestStatusLevelMapping 验证按状态码分级：4xx Warn、5xx Error、2xx Debug。
func TestStatusLevelMapping(t *testing.T) {
	logger, buf := captureLogger(slog.LevelDebug)
	h := logx.Middleware(logx.MiddlewareOptions{Logger: logger})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(409)
		}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/locks/x", nil))

	var entry map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry))
	require.Equal(t, "WARN", entry["level"])
	require.Equal(t, float64(409), entry["status"])
}

// TestNoCredentialLeakInLogs 是最重要的断言：
// 日志里绝不能出现 Authorization 或 token 的任何字符。
func TestNoCredentialLeakInLogs(t *testing.T) {
	logger, buf := captureLogger(slog.LevelDebug)
	const secret = "super-secret-token-value-123456"

	h := logx.Middleware(logx.MiddlewareOptions{Logger: logger})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
		}))

	req := httptest.NewRequest("GET", "/v1/locks", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Tenant-Secret", secret)
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	require.NotContains(t, out, secret, "凭据绝不能出现在日志里")
	require.NotContains(t, out, "Bearer "+secret)
}

// TestRedactHeader 验证头脱敏规则。
func TestRedactHeader(t *testing.T) {
	require.Equal(t, "[REDACTED]", logx.RedactHeader("Authorization", "abc123"))
	require.Equal(t, "Bearer [REDACTED]", logx.RedactHeader("Authorization", "Bearer abc123"))
	require.Equal(t, "[REDACTED]", logx.RedactHeader("X-Tenant-Secret", "abc123"))
	require.Equal(t, "[REDACTED]", logx.RedactHeader("Cookie", "session=abc"))
	require.Equal(t, "application/json", logx.RedactHeader("Content-Type", "application/json"))
	require.Equal(t, "", logx.RedactHeader("Authorization", ""))
}

// TestRedactTokenKeepsNoInformation 验证只报"存在与否"，不泄露长度或前缀。
func TestRedactTokenKeepsNoInformation(t *testing.T) {
	require.Equal(t, "", logx.RedactToken(""))
	require.Equal(t, "[REDACTED]", logx.RedactToken("abc"))
	require.Equal(t, "[REDACTED]", logx.RedactToken("a-very-long-token"))
}

// TestSlowRequestLogged 验证慢请求额外打一条 slow_request。
func TestSlowRequestLogged(t *testing.T) {
	logger, buf := captureLogger(slog.LevelDebug)
	h := logx.Middleware(logx.MiddlewareOptions{
		Logger:        logger,
		SlowThreshold: 10 * time.Millisecond,
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	require.Contains(t, buf.String(), logx.EventSlowRequest)
}

// TestAccessLogHasNoQueryString 验证不记录查询串，
// 因为有些客户端会把凭据塞在 query 里。
func TestAccessLogHasNoQueryString(t *testing.T) {
	logger, buf := captureLogger(slog.LevelDebug)
	h := logx.Middleware(logx.MiddlewareOptions{Logger: logger})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest("GET", "/x?token=leak-me", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.NotContains(t, buf.String(), "leak-me",
		"查询串可能含凭据，不能进日志")
}

// TestRateLimitStatsAggregates 验证限流统计是聚合的，不是逐条灌日志——
// 被攻击时逐条打日志会把日志本身变成攻击面。
func TestRateLimitStatsAggregates(t *testing.T) {
	logger, buf := captureLogger(slog.LevelWarn)
	stats := &logx.RateLimitStats{}
	for i := 0; i < 100; i++ {
		stats.IncIP()
	}
	stats.IncUser()

	stop := make(chan struct{})
	go stats.RunReporter(stop, 10*time.Millisecond, logger)
	time.Sleep(40 * time.Millisecond)
	close(stop)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.LessOrEqual(t, len(lines), 3, "100 次命中不应产生 100 条日志")
	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &entry))
	require.Equal(t, float64(100), entry["by_ip"])
	require.Equal(t, float64(1), entry["by_tenant"])
}

// TestReporterSilentWhenNoHits 验证没有命中时不打空日志。
func TestReporterSilentWhenNoHits(t *testing.T) {
	logger, buf := captureLogger(slog.LevelWarn)
	stats := &logx.RateLimitStats{}
	stop := make(chan struct{})
	go stats.RunReporter(stop, 10*time.Millisecond, logger)
	time.Sleep(40 * time.Millisecond)
	close(stop)
	require.Empty(t, strings.TrimSpace(buf.String()))
}

