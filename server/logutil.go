package server

import (
	"net/http"
	"runtime/debug"
	"strings"
)

// authScheme 只返回认证方案（Bearer / Basic / 空），不返回凭据。
func authScheme(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if v == "" {
		if r.Header.Get("X-Tenant-Secret") != "" {
			return "x-tenant-secret"
		}
		return ""
	}
	if i := strings.IndexByte(v, ' '); i > 0 {
		return v[:i]
	}
	return "unknown"
}

// stackOf 返回当前 goroutine 的调用栈，只在 panic 时用。
func stackOf() string {
	return string(debug.Stack())
}
