package client_test

import (
	"testing"
	"time"

	"github.com/storage-lock/go-remote-storage/client"
	storage_lock "github.com/storage-lock/go-storage-lock"
	"github.com/stretchr/testify/require"
)

// TestRecommendedOptionsFasterThanDefault 锁住这个修复：
// 默认 1 秒的重试间隔是为本地存储设计的，跨公网会让客户端饿死。
func TestRecommendedOptionsFasterThanDefault(t *testing.T) {
	opts := client.RecommendedOptions("k")
	require.Less(t, opts.VersionMissRetryInterval,
		storage_lock.NewStorageLockOptions().VersionMissRetryInterval,
		"远程场景的重试间隔必须比本地默认值短")
	require.Equal(t, 100*time.Millisecond, opts.VersionMissRetryInterval)
}

// TestRecommendedOptionsForRTTScalesWithLatency 验证间隔随 RTT 放大，
// 且下限保证不会在网络上空转。
func TestRecommendedOptionsForRTTScalesWithLatency(t *testing.T) {
	slow := client.RecommendedOptionsForRTT("k", 200*time.Millisecond)
	require.Equal(t, 300*time.Millisecond, slow.VersionMissRetryInterval)

	far := client.RecommendedOptionsForRTT("k", 10*time.Millisecond)
	require.Equal(t, 50*time.Millisecond, far.VersionMissRetryInterval,
		"低于下限时取 50ms，避免空转")
}
