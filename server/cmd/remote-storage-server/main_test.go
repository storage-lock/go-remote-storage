package main

import (
	"strings"
	"testing"
)

// TestSQLiteDSNUsesAmpersand 锁住一个踩过的坑：
// mattn/go-sqlite3 的 DSN 参数分隔符是 "&"，写成 "(" 会让整串参数被静默忽略，
// 表现为高并发下随机 "database is locked"。
func TestSQLiteDSNUsesAmpersand(t *testing.T) {
	dsn := sqliteDSN("/data/locks.db")
	for _, want := range []string{
		"journal_mode=WAL",
		"busy_timeout=5000",
		"synchronous=FULL",
		"txlock=immediate",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN 缺少 %s: %s", want, dsn)
		}
	}
	if strings.Contains(dsn, "(") {
		t.Errorf("DSN 不能含 '('，参数会被忽略: %s", dsn)
	}
	// 参数之间必须用 & 分隔。
	if !strings.Contains(dsn, "wal_autocheckpoint=1000") {
		t.Errorf("DSN 缺少 wal_autocheckpoint，WAL 会无限膨胀: %s", dsn)
	}
	if strings.Count(dsn, "&") != 5 {
		t.Errorf("DSN 应有 5 个 & 分隔符: %s", dsn)
	}
}

// TestSQLiteDSNPreservesExistingQuery 验证已有查询串时用 & 追加。
func TestSQLiteDSNPreservesExistingQuery(t *testing.T) {
	dsn := sqliteDSN("/data/locks.db?cache=shared")
	if !strings.Contains(dsn, "cache=shared&") {
		t.Errorf("已有参数后应改用 & 追加: %s", dsn)
	}
}
