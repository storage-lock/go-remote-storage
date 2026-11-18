package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// BearerAuth 校验 Authorization: Bearer <token>。
//
// 用常量时间比较，避免通过响应时间逐字节猜 token。
type BearerAuth struct {
	Token string
}

func (a BearerAuth) Authenticate(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", errUnauthorized
	}
	given := strings.TrimPrefix(header, prefix)
	if subtle.ConstantTimeCompare([]byte(given), []byte(a.Token)) != 1 {
		return "", errUnauthorized
	}
	return "bearer", nil
}
