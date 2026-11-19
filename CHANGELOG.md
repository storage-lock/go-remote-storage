# Changelog

## v0.1.0

首个版本。

- 线协议 `v1`：capabilities / time / get / create / update / delete / list
- 服务端 `server`：包装任意满足 CAS + 可信时间的 `storage.Storage`，
  不满足则拒绝启动
- 客户端 `client`：实现 `storage.Storage`，能力原样转发、不缓存锁状态、
  不自行重试版本冲突、时间只取服务端
- 可执行程序 `remote-storage-server`（SQLite WAL 后端 + Bearer 鉴权）
- 错误码映射：`version_miss` → `ErrVersionMiss`，
  `lock_not_found` → `ErrLockNotFound`，`lock_exists` → `ErrLockAlreadyExists`
- 测试：真实 HTTP 服务下跑完整锁语义（互斥、并发临界区、FairLock 双实例、
  租约抢占、fencing token 单调、含 `/` 的 lock id、鉴权、协议版本、无 CAS 拒绝）

### 性能

- `List` 改为流式编码：边遍历边写 JSON，不再把全部锁攒进切片再序列化。
  20000 条时实测 RSS 从 **265MB 降到 19MB**（降 93%），响应时间不变（0.17s）。

## v0.1.7

### 新增 `logx` 日志体系

- 访问日志：每个请求一条，含 `request_id` / 方法 / 路径 / 状态 / 字节 / 耗时 / IP。
  按状态码分级：2xx-3xx DEBUG、4xx WARN、5xx ERROR；超过 2s 额外打 `slow_request`。
- `request_id` 回写到 `X-Request-Id` 响应头，便于用户报障时提供；客户端传入的 ID 会沿用。
- 安全事件独立打：`auth_failure` 只记 IP、路径、认证方案，**不记凭据**。
- **凭据绝不进日志**：中间件根本不记录请求头/请求体/查询串，比"先记再脱敏"更可靠。
  有测试断言日志里搜不到完整 token。
- 限流命中聚合统计（`RateLimitStats`），按周期输出。逐条打日志会在被攻击时
  把日志本身变成放大面。
- 日志级别/格式可配：`--log-level` / `--log-format`（json 生产、text 人读）。

## v0.1.6

### 修复与性能

- **WAL 无限膨胀**：默认 `wal_autocheckpoint` 在多连接下不生效，实测 20000 次写入后
  WAL 涨到 166MB 而主库只有 4.7MB。加 `_wal_autocheckpoint=1000` 与
  `_auto_vacuum=incremental` 后，14000 次写入时 WAL 稳定在 4.2MB。
- **`List` 改为流式编码**：20000 条时 RSS 从 265MB 降到 19MB（降 93%），
  响应时间不变（0.17s）。

## v0.1.5

### 修复

- **SQLite 并发参数拼错导致高并发下随机 500**。DSN 分隔符写成了 `(`
  （`?_journal_mode(WAL&_busy_timeout(5000...`），mattn/go-sqlite3 会静默忽略
  整串参数——WAL、busy_timeout、synchronous、txlock 全都没生效。
  实测 3 分钟混合负载 11316 个请求里出现 21 个 `database is locked`。
  改正为 `&` 后 WAL 真正启用（目录下出现 `-wal`/`-shm` 文件）。
- 加回归测试 `TestSQLiteDSNUsesAmpersand`，锁住这个坑。

## v0.1.4

### 新增背压

- `List` 增加条数上限（`MaxListItems`，默认 20000，命令行 `--max-list-items`）。
  超过上限时服务端在响应里标 `truncated: true`，**客户端遇到截断直接报错**，
  不会静默返回不完整列表——否则调用方会以为"锁就只有这么多"。

### 实测

18001 把锁时 List 完整返回 4.7MB / 0.17s；把上限调到 100 后
返回 100 条 + `truncated: true`，客户端明确拒绝。

## v0.1.3

只补测试，不改行为。

- 混沌测试（`chaos_test.go`）：
  - 响应丢失但服务端已处理 → 客户端必须报错，绝不返回成功
  - 服务端响应慢 → 客户端按自己的 ctx 超时返回，不等服务端
  - 16 个并发 Update 抢同一版本 → 恰好 1 个成功
- 网络分区（`partition_test.go`）：每 3 个请求断 1 个，
  6 个客户端反复抢锁，临界区零重叠
- 删除语义（`server/delete_test.go`）：版本不匹配不删、删不存在报 NotFound、
  不带 information_json 也能按版本删
- 健康检查真实性（`server/health_test.go`）：底层 GetTime 失败时返回 503，
  不再无条件回 ok
- 客户端方法全覆盖（`client/methods_test.go`）：Storage 接口每个方法都有断言，
  Init 在服务端关闭后必须失败

覆盖率：client 91.3%，server 75.8%，整体 69.4%（`cmd` 未计）。
