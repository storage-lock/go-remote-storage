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
