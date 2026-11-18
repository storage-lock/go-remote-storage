// Package client 是一个把远程 Storage 服务当作本地 Storage 使用的实现。
//
// 关键约束：这个客户端**不缓存**任何锁状态，也**不重试**版本冲突。
// 每一次 CAS 都必须真的落到服务端，否则本地缓存会让两个客户端都以为自己是持有者。
// 同样地，ErrVersionMiss 必须原样返回给上层——由上层决定重试策略。
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	go_iterator "github.com/golang-infrastructure/go-iterator"
	remote "github.com/storage-lock/go-remote-storage"
	"github.com/storage-lock/go-storage"
)

// Config 是远程存储客户端配置。
type Config struct {
	// BaseURL 服务端地址，例如 https://locks.example.com
	BaseURL string

	// Secret 请求凭据。服务端启用鉴权时必填；放到 Authorization: Bearer 里。
	Secret string

	// HTTPClient 可选，默认带超时的客户端。
	HTTPClient *http.Client

	// Timeout 单次请求超时，默认 10s。
	Timeout time.Duration

	// Name 覆盖 GetName 的返回值，便于日志区分多个远程存储。
	Name string
}

// RemoteStorage 实现 storage.Storage，把操作转发给远程服务。
type RemoteStorage struct {
	baseURL string
	secret  string
	client  *http.Client
	timeout time.Duration
	name    string

	once sync.Once
	caps []storage.StorageCapability
	initErr error
}

var _ storage.Storage = &RemoteStorage{}

// New 构造客户端。它会立刻探测一次服务端能力——
// 把"服务端其实不支持 CAS"这类问题暴露在启动阶段，而不是运行到一半。
func New(ctx context.Context, cfg Config) (*RemoteStorage, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, remote.ErrBaseURLRequired
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("remote storage: base URL must be http(s): %q", base)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
			}
	name := cfg.Name

	rs := &RemoteStorage{
		baseURL: base, secret: cfg.Secret, client: httpClient,
		timeout: timeout, name: name,
	}
	if err := rs.probe(ctx); err != nil {
		return nil, err
	}
	return rs, nil
}

// probe 读取服务端能力并校验协议版本。
//
// 拒绝启动而不是降级：一个"看起来能用"但没有 CAS 的远程存储，
// 会让上层的锁静默失效——这比启动失败危险得多。
func (rs *RemoteStorage) probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()

	var resp remote.CapabilitiesResponse
	if err := rs.do(ctx, http.MethodGet, remote.RouteCapabilities, nil, &resp, true); err != nil {
		return fmt.Errorf("remote storage: probe capabilities: %w", err)
	}
	if resp.ProtocolVersion != remote.ProtocolVersion {
		return fmt.Errorf("%w: server=%q client=%q",
			remote.ErrProtocolVersion, resp.ProtocolVersion, remote.ProtocolVersion)
	}
	if !hasCap(resp.Capabilities, storage.CapabilityCAS) {
		return fmt.Errorf("remote storage %q does not declare CapabilityCAS; "+
			"refusing to use it for locking", resp.Name)
	}
	if !hasCap(resp.Capabilities, storage.CapabilityReliableTime) {
		return fmt.Errorf("remote storage %q does not declare CapabilityReliableTime; "+
			"refusing to use it for locking", resp.Name)
	}
	rs.once.Do(func() {
		rs.caps = resp.Capabilities
		if rs.name == "" {
			rs.name = "remote:" + resp.Name
		}
	})
	return nil
}

func hasCap(caps []storage.StorageCapability, want storage.StorageCapability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// GetName 返回名字，便于在日志里区分多个远程后端。
func (rs *RemoteStorage) GetName() string { return rs.name }

// Capabilities 原样转发服务端的能力声明。
// 客户端不补全、不增强——能力必须是服务端的事。
func (rs *RemoteStorage) Capabilities() []storage.StorageCapability {
	rs.once.Do(func() {})
	return rs.caps
}

// Init 只是重新探测一次，保证服务端仍可达且能力未变。
func (rs *RemoteStorage) Init(ctx context.Context) error {
	return rs.probe(ctx)
}

// Close 本地没有需要释放的资源；保留接口实现。
func (rs *RemoteStorage) Close(ctx context.Context) error { return nil }

// GetTime 取服务端时间。这是锁算法唯一可信的时钟来源，
// 绝不能回退成本机时间——那样多台客户端会在租约过期判断上各算各的。
func (rs *RemoteStorage) GetTime(ctx context.Context) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()
	var resp remote.TimeResponse
	if err := rs.do(ctx, http.MethodGet, remote.RouteTime, nil, &resp, true); err != nil {
		return time.Time{}, err
	}
	return resp.GetTime(), nil
}

// Get 返回锁记录的 JSON 字符串。
func (rs *RemoteStorage) Get(ctx context.Context, lockId string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()
	var resp remote.GetResponse
	if err := rs.do(ctx, http.MethodGet, remote.RouteLock+escape(lockId), nil, &resp, true); err != nil {
		return "", err
	}
	return resp.InformationJSON, nil
}

// CreateWithVersion 要求服务端原子地确认"不存在"再插入。
func (rs *RemoteStorage) CreateWithVersion(ctx context.Context, lockId string,
	version storage.Version, information *storage.LockInformation) error {
	ctx, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()
	body := remote.CreateRequest{Version: version, InformationJSON: information.ToJsonString()}
	return rs.do(ctx, http.MethodPut, remote.RouteLock+escape(lockId), body, nil, true)
}

// UpdateWithVersion 把 CAS 完全交给服务端。
// 这里不做"先 GET 再比较"——那就是把原子性丢在客户端。
func (rs *RemoteStorage) UpdateWithVersion(ctx context.Context, lockId string,
	expectedVersion, newVersion storage.Version, information *storage.LockInformation) error {
	ctx, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()
	body := remote.UpdateRequest{
		ExpectedVersion: expectedVersion,
		NewVersion:      newVersion,
		InformationJSON: information.ToJsonString(),
	}
	return rs.do(ctx, http.MethodPatch, remote.RouteLock+escape(lockId), body, nil, true)
}

// DeleteWithVersion 同样把原子比较交给服务端。
func (rs *RemoteStorage) DeleteWithVersion(ctx context.Context, lockId string,
	expectedVersion storage.Version, information *storage.LockInformation) error {
	ctx, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()
	body := remote.DeleteRequest{
		ExpectedVersion: expectedVersion,
		InformationJSON: information.ToJsonString(),
	}
	return rs.do(ctx, http.MethodDelete, remote.RouteLock+escape(lockId), body, nil, true)
}

// List 把服务端的一次性清单包装成迭代器。
//
// 一次性拉全量的边界：锁数量很大时应改游标分页。当前实现服务于
// "几十到几千把锁"的规模，这在免费公共服务上是合理上限。
func (rs *RemoteStorage) List(ctx context.Context) (go_iterator.Iterator[*storage.LockInformation], error) {
	ctx, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()
	var resp remote.ListResponse
	if err := rs.do(ctx, http.MethodGet, remote.RouteLocks, nil, &resp, true); err != nil {
		return nil, err
	}
	if resp.Truncated {
		// 不静默返回不完整结果：上层会据此以为"只有这么多锁"。
		return nil, fmt.Errorf("remote storage: list truncated at server limit; "+
			"the result is incomplete (%d items), narrow the query or raise the server limit", len(resp.Items))
	}
	items := make([]*storage.LockInformation, 0, len(resp.Items))
	for _, item := range resp.Items {
		info, err := storage.LockInformationFromJsonString(item.InformationJSON)
		if err != nil {
			return nil, fmt.Errorf("remote storage: corrupt record %q: %w", item.LockID, err)
		}
		items = append(items, info)
	}
	return go_iterator.FromSlice(items), nil
}

// do 发一次请求并把错误码还原成 go-storage 的 sentinel error。
func (rs *RemoteStorage) do(ctx context.Context, method, path string,
	body any, out any, auth bool) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("remote storage: encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rs.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("remote storage: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Remote-Storage-Protocol", remote.ProtocolVersion)
	if auth && rs.secret != "" {
		req.Header.Set("Authorization", "Bearer "+rs.secret)
	}

	resp, err := rs.client.Do(req)
	if err != nil {
		return fmt.Errorf("remote storage: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// 非 JSON 响应通常意味着地址指向了别的服务（例如 nginx 错误页、探活页）。
	// 直接报"decode 失败"会让人以为是数据坏了，这里给出可行动的提示。
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("remote storage: %s %s returned non-JSON content-type %q "+
			"(is the base URL pointing at the remote-storage server?): %s",
			method, path, ct, strings.TrimSpace(string(raw)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("remote storage: decode response from %s %s: %w", method, path, err)
	}
	return nil
}

// decodeError 把服务端错误体还原成 sentinel error。
func decodeError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var body remote.ErrorResponse
	if err := json.Unmarshal(raw, &body); err != nil || body.Code == "" {
		return fmt.Errorf("remote storage: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	re := &remote.RemoteError{Code: body.Code, Message: body.Message, Status: resp.StatusCode}
	// 关键：把可重试的版本冲突映射回 ErrVersionMiss，上层的抢占逻辑依赖它。
	if mapped := re.ToStorageError(); mapped != nil && !errors.Is(mapped, re) {
		return mapped
	}
	return re
}

// escape 对 lock id 做路径转义。lock id 可能包含 "/"，
// 但服务端按第一个 "/" 切分 action，所以这里用 QueryEscape 之外的方式：
// 直接把 "/" 编码成 %2F，避免它被当成 action 分隔符。
func escape(id string) string {
	return strings.ReplaceAll(strings.ReplaceAll(id, "%", "%25"), "/", "%2F")
}
