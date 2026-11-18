// Package server 把任意 go-storage Storage 暴露成远程 Storage 服务。
//
// 这个包不理解锁语义：它只把 Storage 的四类原子操作（create/update/delete/get）
// 原样转成 HTTP。锁正确性的全部前提仍由底层 Storage 提供——
// 服务端不"帮忙"做任何补偿，因为补偿一定意味着退化成非原子操作。
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"net/http"
	"strings"
	"time"

	"github.com/storage-lock/go-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
	remote "github.com/storage-lock/go-remote-storage"
)

// Options 是服务端配置。
type Options struct {
	// Storage 是被包装的底层存储。必须声明 CapabilityCAS 与 CapabilityReliableTime，
	// 否则服务端拒绝启动——一个不能提供原子 CAS 的远程存储只会把错误传播给所有客户端。
	Storage storage.Storage

	// Authenticator 校验请求。nil 表示不鉴权（仅适合回环/内网部署）。
	Authenticator Authenticator

	// Logger 默认为 slog 默认 logger。
	Logger *slog.Logger

	// MaxBodyBytes 限制请求体，默认 64KiB。
	MaxBodyBytes int64

	// OperationTimeout 单次存储操作超时，默认 10s。
	OperationTimeout time.Duration

	// MaxListItems 限制 List 一次返回的条数，默认 20000。
	//
	// 一次性拉全量在锁数量很大时会撑爆内存和响应体。超过上限时返回 413 并提示
	// 调用方改用更小的视线范围（当前协议没有分页）。这是一个显式的背压，
	// 好过让服务端 OOM 或者让客户端解析一个几百 MB 的响应。
	MaxListItems int
}

// Authenticator 决定这个请求能否访问存储。返回的 subject 只用于日志。
type Authenticator interface {
	Authenticate(r *http.Request) (subject string, err error)
}

// Handler 实现远程 Storage 的 HTTP 端点。
type Handler struct {
	store    storage.Storage
	auth     Authenticator
	logger   *slog.Logger
	maxBody   int64
	opTimeout time.Duration
	maxList   int
	name     string
	caps     []storage.StorageCapability
}

// New 校验底层存储后构造 Handler。
func New(opts Options) (*Handler, error) {
	if opts.Storage == nil {
		return nil, errors.New("server: storage is required")
	}
	caps := opts.Storage.Capabilities()
	// 远程存储的全部价值就是替客户端保证原子性。底层不满足就直接拒绝启动。
	if !hasCap(caps, storage.CapabilityCAS) {
		return nil, fmt.Errorf("server: storage %q does not declare CapabilityCAS; "+
			"a remote storage without atomic CAS cannot back a lock", opts.Storage.GetName())
	}
	if !hasCap(caps, storage.CapabilityReliableTime) {
		return nil, fmt.Errorf("server: storage %q does not declare CapabilityReliableTime; "+
			"clients would receive an untrustworthy clock", opts.Storage.GetName())
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 64 << 10
	}
	timeout := opts.OperationTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	maxList := opts.MaxListItems
	if maxList <= 0 {
		maxList = 20000
	}
	return &Handler{
		store: opts.Storage, auth: opts.Authenticator, logger: logger,
		maxBody: maxBody, opTimeout: timeout, maxList: maxList,
		name: opts.Storage.GetName(), caps: caps,
	}, nil
}

func hasCap(caps []storage.StorageCapability, want storage.StorageCapability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// Routes 返回完整的 HTTP handler。
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(remote.RouteHealth, h.health)
	// capabilities 也要鉴权：否则未授权的客户端会以为自己连上了，
	// 直到第一次真实操作才失败——把配置错误推迟到运行时。
	mux.HandleFunc(remote.RouteCapabilities, h.requireAuth(h.capabilities))
	// 时间与锁操作都需要鉴权。
	mux.HandleFunc(remote.RouteTime, h.requireAuth(h.time))
	mux.HandleFunc(remote.RouteLocks, h.requireAuth(h.list))
	mux.HandleFunc(remote.RouteLock, h.requireAuth(h.lock))
	return h.recoverPanic(mux)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, remote.CodeBadRequest, "use GET")
		return
	}
	ctx, cancel := h.opCtx(r)
	defer cancel()
	// 健康检查真的打一次存储，而不是只回 ok。
	if _, err := h.store.GetTime(ctx); err != nil {
		h.logger.Error("health check storage error", "err", err)
		writeErr(w, http.StatusServiceUnavailable, remote.CodeInternal, "storage unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// capabilities 不需要鉴权：客户端在建连阶段要先确认协议版本与能力。
func (h *Handler) capabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, remote.CodeBadRequest, "use GET")
		return
	}
	writeJSON(w, http.StatusOK, remote.CapabilitiesResponse{
		ProtocolVersion: remote.ProtocolVersion,
		Name:            h.name,
		Capabilities:    h.caps,
	})
}

func (h *Handler) time(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, remote.CodeBadRequest, "use GET")
		return
	}
	ctx, cancel := h.opCtx(r)
	defer cancel()
	t, err := h.store.GetTime(ctx)
	if err != nil {
		h.writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, remote.TimeResponse{UnixNano: t.UnixNano()})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, remote.CodeBadRequest, "use GET")
		return
	}
	ctx, cancel := h.opCtx(r)
	defer cancel()
	iter, err := h.store.List(ctx)
	if err != nil {
		h.writeStorageError(w, err)
		return
	}

	// 流式编码：不把全部锁攒进一个切片再序列化。
	// 20000 条时攒切片会让峰值内存翻几倍（实测 RSS 从 30MB 涨到 265MB）。
	// 这里边遍历边写，内存只跟单条记录大小有关。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	bw := bufio.NewWriterSize(w, 32<<10)
	defer bw.Flush()

	count := 0
	truncated := false
	_, _ = bw.WriteString(`{"items":[`)
	for iter.Next() {
		info := iter.Value()
		if info == nil {
			continue
		}
		if count >= h.maxList {
			truncated = true
			break
		}
		if count > 0 {
			_ = bw.WriteByte(',')
		}
		item := remote.ListedLock{
			LockID:          info.LockId,
			Version:         info.Version,
			InformationJSON: info.ToJsonString(),
		}
		raw, err := json.Marshal(item)
		if err != nil {
			// 已经写了 200 头和部分内容，无法再改成错误页；记录下来即可。
			h.logger.Error("encode list item", "err", err)
			return
		}
		_, _ = bw.Write(raw)
		count++
	}
	if truncated {
		_, _ = bw.WriteString(`],"truncated":true}`)
		h.logger.Warn("list truncated", "limit", h.maxList)
	} else {
		_, _ = bw.WriteString(`]}`)
	}
}

// lock 处理 /v1/locks/{id}。
//
// 用 EscapedPath 而不是 Path：Go 已经把 Path 解码过，lock id 里的 "/"（被客户端
// 编码成 %2F）会在解码后变成一个真的分隔符，把 id 切坏。
func (h *Handler) lock(w http.ResponseWriter, r *http.Request) {
	escaped := r.URL.EscapedPath()
	path := strings.TrimPrefix(escaped, remote.RouteLock)
	id, action := splitAction(path)
	unescapedID, err := url.PathUnescape(id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, remote.CodeBadRequest, "invalid lock id encoding")
		return
	}
	id = unescapedID
	if id == "" {
		writeErr(w, http.StatusNotFound, remote.CodeBadRequest, "lock id required")
		return
	}
	if len(id) > 255 {
		writeErr(w, http.StatusBadRequest, remote.CodeBadRequest, "lock id too long")
		return
	}
	action, err = url.PathUnescape(action)
	if err != nil {
		writeErr(w, http.StatusBadRequest, remote.CodeBadRequest, "invalid action encoding")
		return
	}
	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			h.get(w, r, id)
		case http.MethodPut:
			h.create(w, r, id)
		case http.MethodPatch:
			h.update(w, r, id)
		case http.MethodDelete:
			h.delete(w, r, id)
		default:
			w.Header().Set("Allow", "GET, PUT, PATCH, DELETE")
			writeErr(w, http.StatusMethodNotAllowed, remote.CodeBadRequest, "unsupported method")
		}
	default:
		writeErr(w, http.StatusNotFound, remote.CodeBadRequest, "unknown action")
	}
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel := h.opCtx(r)
	defer cancel()
	raw, err := h.store.Get(ctx, id)
	if err != nil {
		h.writeStorageError(w, err)
		return
	}
	info, err := storage.LockInformationFromJsonString(raw)
	if err != nil {
		h.logger.Error("stored lock information is corrupt", "lock_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, remote.CodeInternal, "corrupt lock record")
		return
	}
	writeJSON(w, http.StatusOK, remote.GetResponse{
		Version:         info.Version,
		InformationJSON: raw,
	})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, id string) {
	var req remote.CreateRequest
	if !decode(w, r, h.maxBody, &req) {
		return
	}
	info, err := parseInfo(req.InformationJSON)
	if err != nil {
		writeErr(w, http.StatusBadRequest, remote.CodeBadRequest, "invalid information_json: "+err.Error())
		return
	}
	ctx, cancel := h.opCtx(r)
	defer cancel()
	if err := h.store.CreateWithVersion(ctx, id, req.Version, info); err != nil {
		h.writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request, id string) {
	var req remote.UpdateRequest
	if !decode(w, r, h.maxBody, &req) {
		return
	}
	info, err := parseInfo(req.InformationJSON)
	if err != nil {
		writeErr(w, http.StatusBadRequest, remote.CodeBadRequest, "invalid information_json: "+err.Error())
		return
	}
	ctx, cancel := h.opCtx(r)
	defer cancel()
	if err := h.store.UpdateWithVersion(ctx, id, req.ExpectedVersion, req.NewVersion, info); err != nil {
		h.writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request, id string) {
	var req remote.DeleteRequest
	if !decode(w, r, h.maxBody, &req) {
		return
	}
	var info *storage.LockInformation
	if req.InformationJSON != "" {
		var err error
		info, err = parseInfo(req.InformationJSON)
		if err != nil {
			writeErr(w, http.StatusBadRequest, remote.CodeBadRequest, "invalid information_json: "+err.Error())
			return
		}
	}
	ctx, cancel := h.opCtx(r)
	defer cancel()
	if err := h.store.DeleteWithVersion(ctx, id, req.ExpectedVersion, info); err != nil {
		h.writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// writeStorageError 把底层错误映射成线协议错误码。
//
// 映射必须精确：客户端会把 CodeVersionMiss 还原成 ErrVersionMiss，
// 上层据此决定"重试还是失败"。映射错了会让抢占路径死循环。
func (h *Handler) writeStorageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage_lock.ErrVersionMiss):
		writeErr(w, http.StatusConflict, remote.CodeVersionMiss, "version mismatch")
	case errors.Is(err, storage_lock.ErrLockNotFound):
		writeErr(w, http.StatusNotFound, remote.CodeLockNotFound, "lock not found")
	case errors.Is(err, storage_lock.ErrLockAlreadyExists):
		writeErr(w, http.StatusConflict, remote.CodeLockExists, "lock already exists")
	default:
		h.logger.Error("storage operation failed", "err", err)
		writeErr(w, http.StatusInternalServerError, remote.CodeInternal, "storage operation failed")
	}
}

func (h *Handler) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				h.logger.Error("panic recovered", "path", r.URL.Path, "panic", rec)
				writeErr(w, http.StatusInternalServerError, remote.CodeInternal, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requireAuth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.auth == nil {
			fn(w, r)
			return
		}
		subject, err := h.auth.Authenticate(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, remote.CodeUnauthorized, "authentication failed")
			return
		}
		_ = subject
		fn(w, r)
	}
}

func (h *Handler) opCtx(r *http.Request) (ctx context.Context, cancel func()) {
	ctx, cancel = context.WithTimeout(r.Context(), h.opTimeout)
	return
}

func parseInfo(raw string) (*storage.LockInformation, error) {
	if raw == "" {
		return nil, errors.New("empty")
	}
	return storage.LockInformationFromJsonString(raw)
}

func decode(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, remote.CodeBadRequest, "invalid json: "+err.Error())
		return false
	}
	return true
}

func splitAction(path string) (id, action string) {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return path, ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code remote.ErrorCode, msg string) {
	writeJSON(w, status, remote.ErrorResponse{Code: code, Message: msg})
}
