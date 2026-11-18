// Package remote_storage 定义"远程 Storage"的线协议。
//
// 这个包只放协议类型与错误常量，服务端和客户端共用，
// 保证两边对同一份 JSON 的理解不会漂移。
package remote_storage

import (
	"errors"
	"time"

	"github.com/storage-lock/go-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
)

// 协议版本。客户端与服务端的这个值必须一致，否则拒绝启动——
// 静默降级会让"看起来能用"变成实际语义不一致。
const ProtocolVersion = "v1"

// 路由
const (
	// RouteCapabilities 返回服务端 Storage 的能力与名字。
	RouteCapabilities = "/v1/capabilities"
	// RouteTime 返回服务端时间（单调来源由服务端保证）。
	RouteTime = "/v1/time"
	// RouteLock 是 /v1/locks/{lockId} 的前缀。
	RouteLock = "/v1/locks/"
	// RouteLocks 列出全部锁。
	RouteLocks = "/v1/locks"
	// RouteHealth 健康检查。
	RouteHealth = "/v1/health"
)

// 错误码。HTTP 状态码只是传输层，语义必须由 code 表达，
// 否则客户端无法区分"版本冲突可重试"和"记录不存在是硬错误"。
type ErrorCode string

const (
	CodeVersionMiss     ErrorCode = "version_miss"
	CodeLockNotFound    ErrorCode = "lock_not_found"
	CodeLockExists      ErrorCode = "lock_exists"
	CodeUnauthorized    ErrorCode = "unauthorized"
	CodeRateLimited     ErrorCode = "rate_limited"
	CodeBadRequest      ErrorCode = "bad_request"
	CodeInternal        ErrorCode = "internal"
	CodeProtocolVersion ErrorCode = "protocol_version"
)

// ErrorResponse 是任何非 2xx 响应的 body。
type ErrorResponse struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message,omitempty"`
}

// RemoteError 是客户端侧还原出来的错误。
type RemoteError struct {
	Code    ErrorCode
	Message string
	Status  int
}

func (e *RemoteError) Error() string {
	if e.Message != "" {
		return "remote storage: " + string(e.Code) + ": " + e.Message
	}
	return "remote storage: " + string(e.Code)
}

// ToStorageError 把线协议错误映射回 go-storage 期望的 sentinel error。
//
// 这一步至关重要：上层 go-storage-lock 只把 ErrVersionMiss 当成"可重试"，
// 把 ErrLockNotFound 当成硬错误。如果这里映射错了，抢占路径会死循环或直接失败。
func (e *RemoteError) ToStorageError() error {
	switch e.Code {
	case CodeVersionMiss:
		return storage_lock.ErrVersionMiss
	case CodeLockNotFound:
		return storage_lock.ErrLockNotFound
	case CodeLockExists:
		return storage_lock.ErrLockAlreadyExists
	default:
		return e
	}
}

// CapabilitiesResponse 描述服务端 Storage 的自我声明。
//
// 客户端会**原样转发**这些能力，而不是自己补全——能力必须是服务端的事，
// 客户端无权替它背书。
type CapabilitiesResponse struct {
	ProtocolVersion string                        `json:"protocol_version"`
	Name            string                        `json:"name"`
	Capabilities    []storage.StorageCapability   `json:"capabilities"`
}

// TimeResponse 是服务端时间。
type TimeResponse struct {
	// UnixNano 服务端单调时间的纳秒表示。
	UnixNano int64 `json:"unix_nano"`
}

// GetTime 还原成 time.Time。
func (r TimeResponse) GetTime() time.Time {
	return time.Unix(0, r.UnixNano).UTC()
}

// LockValue 是单个锁记录的存储形态。
//
// 这里刻意只用"版本 + JSON 字符串"，与 Storage 接口的 Get 返回类型对齐，
// 避免服务端和客户端对 LockInformation 结构各有一份理解。
type LockValue struct {
	Version           storage.Version `json:"version"`
	InformationJSON   string          `json:"information_json"`
}

// CreateRequest 是 CreateWithVersion 的请求体。
type CreateRequest struct {
	Version         storage.Version `json:"version"`
	InformationJSON string          `json:"information_json"`
}

// UpdateRequest 是 UpdateWithVersion 的请求体。
type UpdateRequest struct {
	ExpectedVersion storage.Version `json:"expected_version"`
	NewVersion      storage.Version `json:"new_version"`
	InformationJSON string          `json:"information_json"`
}

// DeleteRequest 是 DeleteWithVersion 的请求体。
//
// 带上 InformationJSON 是为了与接口签名一致；服务端不会用它做校验，
// 语义完全由 ExpectedVersion 决定。
type DeleteRequest struct {
	ExpectedVersion storage.Version `json:"expected_version"`
	InformationJSON string          `json:"information_json,omitempty"`
}

// GetResponse 是 Get 的响应体。
type GetResponse struct {
	Version         storage.Version `json:"version"`
	InformationJSON string          `json:"information_json"`
}

// ListResponse 是 List 的响应体。服务端一次性返回全部锁；
// 对锁数量极大的部署应先改成游标分页（见 README 的边界说明）。
type ListResponse struct {
	Items []ListedLock `json:"items"`
	// Truncated 为真表示服务端达到了返回条数上限，结果不完整。
	Truncated bool `json:"truncated,omitempty"`
}

// ListedLock 是 List 里的一项。
type ListedLock struct {
	LockID          string          `json:"lock_id"`
	Version         storage.Version `json:"version"`
	InformationJSON string          `json:"information_json"`
}

// 客户端侧的配置错误。
var (
	ErrBaseURLRequired = errors.New("remote storage: base URL is required")
	ErrProtocolVersion = errors.New("remote storage: protocol version mismatch")
)
