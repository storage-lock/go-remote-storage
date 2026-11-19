package metrics

import (
	"net/http"
	"time"
)

// HTTPMiddleware 记录 HTTP 层指标。
// 放在访问日志中间件的内层：先记录指标，再由外层写日志。
func HTTPMiddleware(reg *Registry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reg.InFlightInc()
		defer reg.InFlightDec()

		rec := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		reg.ObserveHTTP(r.Method, RouteLabel(r.URL.Path), status,
			time.Since(start), rec.bytes)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += int64(n)
	return n, err
}

// Flush 透传，保证流式响应不被缓冲。
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
