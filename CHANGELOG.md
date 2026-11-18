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
