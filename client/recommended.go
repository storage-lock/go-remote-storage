package client

import (
	"time"

	storage_lock "github.com/storage-lock/go-storage-lock"
)

// RecommendedOptions 返回适合"跨网络远程存储"的锁选项。
//
// 为什么需要它：`go-storage-lock` 的默认 VersionMissRetryInterval 是 1 秒，
// 这个值是为本地存储（微秒级 CAS）设计的。跨公网时单次 CAS 往返就要几十毫秒，
// 多个客户端争抢时，抢失败的一方要等 1 秒才重试——30 秒的 Lock 超时只够试 30 次，
// 高并发下会有客户端饿死（实测 8 进程跨公网时有 2 个超时）。
//
// 这里把重试间隔降到 100ms 量级：既远大于单次网络往返（避免空转），
// 又能在合理时间内重试足够多次。
//
// 注意：重试间隔不能小于单次 CAS 往返，否则会在网络上空转，反而更难抢到。
// 如果客户端到服务端的往返超过 100ms，请按实测值调大（一般取 1~2 倍 RTT）。
func RecommendedOptions(lockId string) *storage_lock.StorageLockOptions {
	opts := storage_lock.NewStorageLockOptionsWithLockId(lockId)
	opts.VersionMissRetryInterval = 100 * time.Millisecond
	return opts
}

// RecommendedOptionsForRTT 依据实测往返时间给出重试间隔。
// rtt 建议用几次 GetTime 的平均值；间隔取 RTT 的 1.5 倍并下限 50ms。
func RecommendedOptionsForRTT(lockId string, rtt time.Duration) *storage_lock.StorageLockOptions {
	interval := time.Duration(float64(rtt) * 1.5)
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	opts := storage_lock.NewStorageLockOptionsWithLockId(lockId)
	opts.VersionMissRetryInterval = interval
	return opts
}
