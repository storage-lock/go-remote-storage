package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// newLogger 构造日志器。
//
// 默认 json 格式：生产环境用日志采集器（Loki/ELK）解析 JSON 比解析文本可靠。
// text 只用于本地人读调试。
func newLogger(level, format string) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "info", "":
		lv = slog.LevelInfo
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log level %q (want debug/info/warn/error)", level)
	}

	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json", "":
		h = slog.NewJSONHandler(os.Stdout, opts)
	case "text":
		h = slog.NewTextHandler(os.Stdout, opts)
	default:
		return nil, fmt.Errorf("invalid log format %q (want json/text)", format)
	}
	return slog.New(h), nil
}
