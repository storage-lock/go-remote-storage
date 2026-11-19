package logx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// 日志体系设计要点
//
// 1. **凭据绝不出现在日志里**。Bearer token / X-Tenant-Secret 一旦进日志，
//    任何能读日志的人都能冒充所有用户。这里统一在入口做脱敏。
// 2. **每个请求有 request_id**，跨中间件和存储层可关联。
// 3. **访问日志分级**：正常请求 Debug，4xx Warn，5xx Error——
//    这样默认 Info 级别下不会刷屏，但异常一定看得见。
// 4. **安全事件单独打**：鉴权失败、限流命中用固定 event 名，
//    方便做告警规则，不用靠正则去猜普通日志。
// 5. **限流器不能靠日志排查**：主动统计计数，定期输出，避免逐条打日志。

const (
	// Event 名，用于日志检索与告警规则。
	EventAuthFailure   = "auth_failure"
	EventRateLimited   = "rate_limited"
	EventStorageError  = "storage_error"
	EventPanic         = "panic_recovered"
	EventListTruncated = "list_truncated"
	EventSlowRequest   = "slow_request"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

// WithRequestID 把请求 ID 放进 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom 取出请求 ID，没有则返回空。
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// NewRequestID 生成 16 字节的随机十六进制 ID。
func NewRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf)
}

// redactedPlaceholder 是脱敏后的固定字符串。
// 不要用长度或前缀，那也会泄露信息。
const redactedPlaceholder = "[REDACTED]"

// RedactHeader 返回脱敏后的头值，用于日志。
func RedactHeader(name, value string) string {
	if value == "" {
		return ""
	}
	switch strings.ToLower(name) {
	case "authorization", "x-tenant-secret", "cookie", "set-cookie", "proxy-authorization":
		// 保留认证方案（Bearer/Basic），只把凭据部分打码。
		if i := strings.IndexByte(value, ' '); i > 0 {
			return value[:i] + " " + redactedPlaceholder
		}
		return redactedPlaceholder
	}
	return value
}

// RedactToken 用于单个 token 值：只保留是否存在的信息，不保留任何字符。
func RedactToken(v string) string {
	if v == "" {
		return ""
	}
	return redactedPlaceholder
}

// statusRecorder 记录响应状态码与字节数，供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Flush 透传给底层，保证流式响应（List / SSE）不被缓冲。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware 是最外层的请求日志中间件：
//   - 生成并注入 request_id
//   - 记录方法/路径/状态/耗时/字节数/客户端 IP
//   - 按状态码分级：2xx/3xx Debug、4xx Warn、5xx Error
//   - 超过阈值的请求额外打一条 slow_request
//
// 注意：这里**不记录请求头与请求体**。凭据脱敏是靠"根本不记录"来保证的，
// 比先记录再脱敏更可靠。
type MiddlewareOptions struct {
	Logger *slog.Logger
	// SlowThreshold 超过这个耗时额外打 slow_request，默认 2s。
	SlowThreshold time.Duration
	// ClientIP 从请求取真实 IP；保证与限流用的是同一个口径。
	ClientIP func(*http.Request) string
	// OnSlow 在判定为慢请求时回调，供指标层计数。可为 nil。
	OnSlow func()
}

func Middleware(opts MiddlewareOptions) func(http.Handler) http.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	slow := opts.SlowThreshold
	if slow <= 0 {
		slow = 2 * time.Second
	}
	ipOf := opts.ClientIP
	if ipOf == nil {
		ipOf = func(r *http.Request) string { return r.RemoteAddr }
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reqID := r.Header.Get("X-Request-Id")
			if reqID == "" || len(reqID) > 64 {
				reqID = NewRequestID()
			}
			// 回写给客户端，便于用户报障时提供。
			w.Header().Set("X-Request-Id", reqID)
			r = r.WithContext(WithRequestID(r.Context(), reqID))

			rec := &statusRecorder{ResponseWriter: w}
			defer func() {
				elapsed := time.Since(start)
				status := rec.status
				if status == 0 {
					status = http.StatusOK
				}
				attrs := []any{
					"event", "request",
					"request_id", reqID,
					"method", r.Method,
					"path", r.URL.Path,
					"status", status,
					"bytes", rec.bytes,
					"duration_ms", elapsed.Milliseconds(),
					"ip", ipOf(r),
				}
				switch {
				case status >= 500:
					logger.Error("request", attrs...)
				case status >= 400:
					logger.Warn("request", attrs...)
				default:
					logger.Debug("request", attrs...)
				}
				if elapsed > slow {
					if opts.OnSlow != nil {
						opts.OnSlow()
					}
					logger.Warn("slow request", append(attrs,
						"event", EventSlowRequest,
						"threshold_ms", slow.Milliseconds())...)
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// RateLimitStats 汇总限流命中，按固定周期输出而不是逐条打日志。
// 逐条打会在被攻击时把日志本身变成放大攻击面。
type RateLimitStats struct {
	ipHits   atomic.Int64
	userHits atomic.Int64
}

func (s *RateLimitStats) IncIP()   { s.ipHits.Add(1) }
func (s *RateLimitStats) IncUser() { s.userHits.Add(1) }

// Report 取走并清零当前计数。
func (s *RateLimitStats) Report() (ip, user int64) {
	ip = s.ipHits.Swap(0)
	user = s.userHits.Swap(0)
	return
}

// RunReporter 每 interval 输出一次限流统计，直到 stop 关闭。
func (s *RateLimitStats) RunReporter(stop <-chan struct{}, interval time.Duration, logger *slog.Logger) {
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			ip, user := s.Report()
			if ip == 0 && user == 0 {
				continue // 没命中就不打，避免空日志
			}
			logger.Warn("rate limit hits",
				"event", EventRateLimited,
				"interval", interval.String(),
				"by_ip", ip,
				"by_tenant", user)
		}
	}
}
