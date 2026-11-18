// remote-storage-server 把本地存储暴露成远程 Storage 服务。
//
// 典型部署：一台 VPS 跑它（SQLite 落盘），任意机器上的进程用 client 连过来当本地存储用。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/storage-lock/go-storage"
	memory_storage "github.com/storage-lock/go-memory-storage"
	sqlite3_storage "github.com/storage-lock/go-sqlite3-storage"
	"github.com/storage-lock/go-remote-storage/server"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", envOr("REMOTE_STORAGE_ADDR", "127.0.0.1:8443"), "监听地址")
	data := flag.String("data", envOr("REMOTE_STORAGE_DATA", "remote-locks.db"), "SQLite 数据路径；空表示仅内存（重启即丢）")
	token := flag.String("token", envOr("REMOTE_STORAGE_TOKEN", ""), "访问令牌；为空表示不鉴权（仅适合回环/内网）")
	tlsCert := flag.String("tls-cert", os.Getenv("REMOTE_STORAGE_TLS_CERT"), "TLS 证书")
	tlsKey := flag.String("tls-key", os.Getenv("REMOTE_STORAGE_TLS_KEY"), "TLS 私钥")
	maxList := flag.Int("max-list-items", 20000, "List 一次返回的最大条数；超过时标记 truncated")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	store, closer, err := openStorage(context.Background(), *data)
	if err != nil {
		return err
	}
	defer closer()

	var auth server.Authenticator
	if strings.TrimSpace(*token) != "" {
		auth = server.BearerAuth{Token: *token}
	} else {
		logger.Warn("未设置 REMOTE_STORAGE_TOKEN，服务无鉴权；只应监听回环或内网")
	}

	h, err := server.New(server.Options{
		Storage: store, Authenticator: auth, Logger: logger,
		MaxListItems: *maxList,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           h.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("remote-storage-server listening", "addr", *addr, "data", *data,
			"tls", *tlsCert != "", "auth", auth != nil)
		var err error
		if *tlsCert != "" && *tlsKey != "" {
			err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func openStorage(ctx context.Context, path string) (storage.Storage, func(), error) {
	if strings.TrimSpace(path) == "" {
		return memory_storage.NewMemoryStorage(), func() {}, nil
	}
	opts := sqlite3_storage.NewSqlite3StorageOptions()
	opts.TableName = "remote_locks"
	cm, err := sqlite3_storage.NewSqlite3ConnectionManager(sqliteDSN(path))
	if err != nil {
		return nil, nil, err
	}
	opts.ConnectionManager = cm
	store, err := sqlite3_storage.NewSqlite3Storage(ctx, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	return store, func() { _ = store.Close(context.Background()) }, nil
}

// sqliteDSN 给数据文件路径追加并发相关参数。
//
// 参数之间必须用 "&"：写成 "(" 会让 mattn/go-sqlite3 静默忽略整串参数，
// 表现为高并发下随机 "database is locked"。有一次压测 11316 个请求出现 21 个 500，
// 就是这里拼错导致的。
func sqliteDSN(path string) string {
	const params = "_journal_mode=WAL&_busy_timeout=5000&_synchronous=FULL&_txlock=immediate"
	if strings.Contains(path, "?") {
		return path + "&" + params
	}
	return path + "?" + params
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
