// Package metrics 提供服务端的 Prometheus 指标。
//
// 设计约束：
//
//  1. **低基数**。label 只能用固定枚举（方法、状态码类、结果、存储名），
//     绝不能用 lock_id / owner_id / IP / 请求 ID——那些会让指标爆炸，
//     轻则 Prometheus 内存暴涨，重则直接把时序库打挂。
//  2. **指标是给运维看的**，不是给业务看。所以只统计聚合量，不统计单把锁。
//  3. **必须能和日志对上**。同一件事在日志里用 event 名，在指标里用对应计数器，
//     排障时可以从告警跳到指标再跳到日志。
//  4. **不伪造"当前持有锁数量"**。远程存储服务不知道自己是不是唯一写入方，
//     给出这种 gauge 会误导。只给"我处理了多少次请求"这类真实可观测的量。
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry 持有全部指标。一个进程一个实例。
type Registry struct {
	reg *prometheus.Registry

	httpRequests   *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	httpInFlight   prometheus.Gauge
	responseBytes  *prometheus.CounterVec

	storageOps     *prometheus.CounterVec
	storageLatency *prometheus.HistogramVec
	storageErrors  *prometheus.CounterVec

	rateLimited *prometheus.CounterVec
	authFail    prometheus.Counter

	listTruncated prometheus.Counter
	panics        prometheus.Counter
	slowRequests  prometheus.Counter

	buildInfo *prometheus.GaugeVec
}

// 固定标签值集合。写成常量避免各处拼字符串拼错。
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// New 创建并注册全部指标。
//
// namespace 用来区分本服务在共享 Prometheus 里的前缀，默认 remote_storage。
func New(namespace string) *Registry {
	if namespace == "" {
		namespace = "remote_storage"
	}
	r := &Registry{reg: prometheus.NewRegistry()}

	r.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "http_requests_total",
		Help:      "HTTP 请求总数，按方法、路径类别、状态码类分组。",
	}, []string{"method", "route", "status_class"})

	r.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "http_request_duration_seconds",
		Help:      "HTTP 请求耗时分布。",
		// 从 1ms 到 10s，覆盖本地到跨公网的全部范围。
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"method", "route", "status_class"})

	r.httpInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "http_in_flight_requests",
		Help:      "当前正在处理的请求数。",
	})

	r.responseBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "http_response_bytes_total",
		Help:      "响应体总字节数，可用于发现异常大的 List 响应。",
	}, []string{"route"})

	r.storageOps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "storage_operations_total",
		Help:      "底层存储操作总数。",
	}, []string{"operation", "result"})

	r.storageLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "storage_operation_duration_seconds",
		Help:      "底层存储操作耗时。",
		Buckets:   []float64{0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	}, []string{"operation"})

	r.storageErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "storage_errors_total",
		Help:      "底层存储错误，按语义分类（version_miss 是可重试的，不算故障）。",
	}, []string{"kind"})

	r.rateLimited = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rate_limited_total",
		Help:      "被限流拒绝的请求数。",
	}, []string{"scope"})

	r.authFail = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "auth_failures_total",
		Help:      "鉴权失败次数。持续非零要排查是配置错误还是被扫。",
	})

	r.listTruncated = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "list_truncated_total",
		Help:      "List 因达到上限而截断的次数。非零说明该扩容或改分页了。",
	})

	r.panics = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "panics_total",
		Help:      "请求处理中 recover 掉的 panic 次数。任何非零都是 bug。",
	})

	r.slowRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "slow_requests_total",
		Help:      "超过慢请求阈值的请求数。",
	})

	r.buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "构建信息，恒为 1，label 携带版本。",
	}, []string{"version"})

	r.reg.MustRegister(
		r.httpRequests, r.httpDuration, r.httpInFlight, r.responseBytes,
		r.storageOps, r.storageLatency, r.storageErrors,
		r.rateLimited, r.authFail, r.listTruncated, r.panics, r.slowRequests,
		r.buildInfo,
	)
	return r
}

// Handler 返回 /metrics 的 handler。
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		// 指标采集本身出错也要能返回部分数据，不要让监控变成单点故障。
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// SetVersion 设置 build_info 的版本 label。
func (r *Registry) SetVersion(v string) {
	r.buildInfo.WithLabelValues(v).Set(1)
}

// ObserveHTTP 记录一次 HTTP 请求。
func (r *Registry) ObserveHTTP(method, route string, status int, dur time.Duration, bytes int64) {
	class := statusClass(status)
	r.httpRequests.WithLabelValues(method, route, class).Inc()
	r.httpDuration.WithLabelValues(method, route, class).Observe(dur.Seconds())
	r.responseBytes.WithLabelValues(route).Add(float64(bytes))
}

func (r *Registry) InFlightInc() { r.httpInFlight.Inc() }
func (r *Registry) InFlightDec() { r.httpInFlight.Dec() }

// ObserveStorage 记录一次底层存储操作。
func (r *Registry) ObserveStorage(operation string, err error, dur time.Duration) {
	result := ResultOK
	if err != nil {
		result = ResultError
	}
	r.storageOps.WithLabelValues(operation, result).Inc()
	r.storageLatency.WithLabelValues(operation).Observe(dur.Seconds())
}

// IncStorageError 记录存储错误的语义类别。
// version_miss 是正常的重试信号，单独归类，避免和真实故障混在一起。
func (r *Registry) IncStorageError(kind string) {
	r.storageErrors.WithLabelValues(kind).Inc()
}

// IncRateLimited 记录限流命中，scope 为 "ip" 或 "tenant"。
func (r *Registry) IncRateLimited(scope string) {
	r.rateLimited.WithLabelValues(scope).Inc()
}

func (r *Registry) IncAuthFailure()        { r.authFail.Inc() }
func (r *Registry) IncListTruncated()      { r.listTruncated.Inc() }
func (r *Registry) IncPanic()              { r.panics.Inc() }
func (r *Registry) IncSlowRequest()        { r.slowRequests.Inc() }

// statusClass 把具体状态码归成 2xx/3xx/4xx/5xx。
// 直接用原始状态码会产生大量时序，而运维关心的只是"哪一类错了"。
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	case status >= 100:
		return "1xx"
	default:
		return strconv.Itoa(status)
	}
}

// RouteLabel 把具体路径归一化成固定的 route 名。
//
// 这是低基数的关键：/v1/locks/{任意 id} 必须归成 /v1/locks/{id}，
// 否则每个 lock id 都会生成一条新的时序。
func RouteLabel(path string) string {
	switch {
	case path == "/v1/health" || path == "/health":
		return "/health"
	case contains(path, "/v1/locks/"):
		return "/v1/locks/{id}"
	case contains(path, "/v1/locks"):
		return "/v1/locks"
	case contains(path, "/v1/time"):
		return "/v1/time"
	case contains(path, "/v1/capabilities"):
		return "/v1/capabilities"
	case contains(path, "/api/v1/locks/"):
		return "/api/v1/locks/{id}"
	case contains(path, "/api/v1/locks"):
		return "/api/v1/locks"
	case contains(path, "/api/v1/tenant"):
		return "/api/v1/tenant"
	case contains(path, "/api/v1/endpoints"):
		return "/api/v1/endpoints"
	case contains(path, "/metrics"):
		return "/metrics"
	default:
		// 未知路径归为一类，防止扫描器用随机路径把基数打爆。
		return "other"
	}
}

func contains(s, sub string) bool {
	return len(sub) <= len(s) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
