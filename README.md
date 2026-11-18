# go-remote-storage

把 `go-storage` 的 `Storage` 接口通过 HTTPS 暴露出来，让**任意机器上的任意进程**
把一台 VPS 上的锁存储当成自己的本地存储用。

## 为什么需要它

`go-storage-lock` 的正确性依赖存储提供**线性一致的 CAS**和**可信时间源**。
你有三种选择：

1. 每个客户端都连同一个 Redis/PostgreSQL/etcd —— 需要运维数据库、凭证分发、网络打通；
2. 每个客户端各用各的 SQLite/内存 —— **锁不成立**，两个进程各算各的；
3. 一台 VPS 跑这个服务，其他所有机器连过来 —— **本项目**。

第 3 种把"需要运维一个数据库"降级成"需要一台能跑二进制的小机器"，
同时保留了第 1 种的全部语义。

## 架构

```
┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│ 客户端 A    │  │ 客户端 B    │  │ 客户端 C    │
│ RemoteStorage│ │ RemoteStorage│ │ RemoteStorage│
└──────┬──────┘  └──────┬──────┘  └──────┬──────┘
       │  HTTPS + Bearer token          │
       └───────────────┬────────────────┘
                       ▼
          ┌────────────────────────┐
          │ remote-storage-server  │  ← 一台 VPS
          │  (只做协议转换)         │
          └───────────┬────────────┘
                      ▼
          ┌────────────────────────┐
          │ SQLite (WAL)           │  ← 唯一事实来源
          └────────────────────────┘
```

服务端**不理解锁语义**。它只把四类原子操作（create / update / delete / get）
原样转发给底层 Storage。锁算法全部在客户端，由 `go-storage-lock` 完成。

这一点很重要：服务端不做任何"补偿"，因为补偿必然意味着退化成非原子操作。

## 快速开始

### 1. 在 VPS 上起服务

```bash
go install github.com/storage-lock/go-remote-storage/server/cmd/remote-storage-server@latest

export REMOTE_STORAGE_TOKEN=$(head -c 32 /dev/urandom | base64)
remote-storage-server \
  --addr 127.0.0.1:8443 \
  --data /var/lib/remote-storage/locks.db \
  --token "$REMOTE_STORAGE_TOKEN"
```

服务会校验底层存储是否声明了 `CapabilityCAS` 与 `CapabilityReliableTime`，
不满足就**拒绝启动**——一个不能提供原子 CAS 的远程存储只会把错误传播给所有客户端。

### 2. 在客户端使用

```go
import (
    "github.com/storage-lock/go-remote-storage/client"
    storage_lock "github.com/storage-lock/go-storage-lock"
)

store, err := client.New(ctx, client.Config{
    BaseURL: "https://locks.example.com",
    Secret:  os.Getenv("REMOTE_STORAGE_TOKEN"),
})
if err != nil {
    log.Fatal(err)   // 探测阶段就会暴露连不上/无权限/协议不匹配
}

lock, err := storage_lock.NewStorageLock(store, "order-42")
owner := lock.NewOwnerId()
err = lock.Lock(ctx, owner)
```

之后的一切与本地存储完全一样：`TryLock`、`Renew`、读写锁、`FairLock`、
`WithLock`、fencing token、看门狗都可用。

## 协议

| 方法 | 路径 | 对应 Storage 方法 |
|---|---|---|
| GET | `/v1/capabilities` | `Capabilities()` / `GetName()` |
| GET | `/v1/health` | — |
| GET | `/v1/time` | `GetTime()` |
| GET | `/v1/locks/{id}` | `Get()` |
| PUT | `/v1/locks/{id}` | `CreateWithVersion()` |
| PATCH | `/v1/locks/{id}` | `UpdateWithVersion()` |
| DELETE | `/v1/locks/{id}` | `DeleteWithVersion()` |
| GET | `/v1/locks` | `List()` |

错误用**语义码**而不是只看 HTTP 状态：

| code | HTTP | 还原成 |
|---|---|---|
| `version_miss` | 409 | `ErrVersionMiss`（可重试） |
| `lock_not_found` | 404 | `ErrLockNotFound`（硬错误） |
| `lock_exists` | 409 | `ErrLockAlreadyExists` |

这个映射必须精确。上层 `go-storage-lock` 的抢占逻辑只把 `ErrVersionMiss`
当成"可以重试"，映射错了会让抢占路径死循环或直接失败。

## 跨网络要调重试间隔

`go-storage-lock` 默认 `VersionMissRetryInterval` 是 1 秒——为本地存储设计。
跨公网时单次 CAS 往返几十毫秒，多客户端争抢会让抢不到的一方等 1 秒才重试，
30 秒超时内试不了几次，会**饿死**。

实测：8 进程跨公网抢同一把锁，用默认值时有 2 个进程 30 秒超时；
改用推荐值后 80/80 全部成功、零重叠。

```go
// 固定 100ms（适合 RTT < 70ms）
opts := client.RecommendedOptions("order-42")

// 按实测 RTT 自适应（取 1.5 倍，下限 50ms）
rtt := measureRTT(store)          // 用几次 GetTime 的平均值
opts := client.RecommendedOptionsForRTT("order-42", rtt)

lock, err := storage_lock.NewStorageLockWithOptions(store, opts)
```

间隔不能小于单次 CAS 往返，否则会在网络上空转。

## 设计约束（都有测试覆盖）

**能力必须来自服务端。** 客户端 `Capabilities()` 原样转发服务端的声明，
不补全、不增强。能力是服务端的事，客户端无权替它背书。

**客户端不缓存锁状态。** 每次 CAS 都真的打到服务端。本地缓存会让两个客户端
都以为自己持有锁——这正是分布式锁最典型的失效方式。

**客户端不自己重试版本冲突。** `ErrVersionMiss` 原样返回，由上层决定策略。

**时间只来自服务端。** `GetTime()` 必须走 `/v1/time`，绝不回退本机时钟。
否则不同客户端在租约过期判断上各算各的。

**协议版本必须一致。** 不匹配就拒绝启动，不静默降级。

## 已验证

`go test ./...` 用真实 HTTP 服务（`httptest`）跑完整锁语义：

- 基础互斥
- 8 并发 × 10 轮临界区零重叠
- `FairLock` 双实例互斥（v0.0.7 那个 P0 漏洞的场景）
- 租约过期后可被抢占
- fencing token 单调递增
- 含 `/` 的 lock id 不被切坏
- 无 token 拒绝建连、协议版本不匹配拒绝启动
- 底层无 CAS 的存储被拒绝启动

## 部署到 VPS

一键安装（systemd）：

```bash
sudo git clone https://github.com/storage-lock/go-remote-storage.git
cd go-remote-storage && sudo ./deploy/install.sh
```

脚本会建专用用户、生成 token、编译安装、装 systemd unit（含 `MemoryMax=256M`
与 `ProtectSystem=strict` 等加固项），然后打印 nginx/TLS 下一步。

Docker：`docker build -f deploy/Dockerfile -t remote-storage .`，
数据挂卷到 `/data`，token 通过 `REMOTE_STORAGE_TOKEN` 固定。
Docker Hub 拉不动时用 `--build-arg RUNTIME_BASE=nginx:1.27-alpine`。

nginx 反代模板在 `deploy/nginx.conf`（强制 TLS + HSTS + 覆写 XFF）。

## 完整产品链路

```bash
# 1. 存储服务（唯一事实来源）
remote-storage-server --addr 127.0.0.1:8443 --data /var/lib/remote-storage/locks.db --token "$TOKEN"

# 2. 对外锁 API（storage-lock-cloud-api）
storage-lock-cloud-api --remote https://127.0.0.1:8443 --remote-token "$TOKEN"

# 3. nginx 终止 TLS，公网只开 443
```

`storage-lock-cloud-api` 的 `--remote` 参数就是为这个准备的：
配了它就不再用本地 SQLite，而是把 `go-remote-storage` 当后端。
这样同一台 VPS 可以同时对外提供锁 API，也可以被自己的其他服务直接用。

## 边界

- `List()` 一次性返回全部锁。规模在几十到几千把锁时没问题；
  更大规模需要改游标分页。
- 单个服务端是单点。要高可用，需要把底层换成 PostgreSQL/etcd 并跑多个
  `remote-storage-server` 实例——它们必须连**同一个**底层存储。
- 传输层必须 TLS。`--addr 127.0.0.1:8443` 是刻意的：公网入口交给 nginx，
  不要在公网直接暴露明文端口，token 会以明文传输。
