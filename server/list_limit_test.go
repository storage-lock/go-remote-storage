package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	memory_storage "github.com/storage-lock/go-memory-storage"
	"github.com/storage-lock/go-remote-storage/client"
	"github.com/storage-lock/go-remote-storage/server"
	"github.com/storage-lock/go-storage"
	"github.com/stretchr/testify/require"
)

func listLimitServer(t *testing.T, limit int) *httptest.Server {
	t.Helper()
	h, err := server.New(server.Options{
		Storage:      memory_storage.NewMemoryStorage(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxListItems: limit,
	})
	require.NoError(t, err)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv
}

// TestListTruncatesAtLimit 验证超过上限时服务端标记截断，
// 而不是返回半个列表还装作完整。
func TestListTruncatesAtLimit(t *testing.T) {
	srv := listLimitServer(t, 3)
	rs, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.NoError(t, err)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		require.NoError(t, rs.CreateWithVersion(ctx, id, 1,
			&storage.LockInformation{LockId: id, OwnerId: "o", Version: 1}))
	}

	// 客户端必须报错，不能静默返回不完整结果。
	_, err = rs.List(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "truncated")
}

// TestListUnderLimitIsComplete 验证没到上限时正常返回。
func TestListUnderLimitIsComplete(t *testing.T) {
	srv := listLimitServer(t, 10)
	rs, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, rs.CreateWithVersion(ctx, "x", 1,
		&storage.LockInformation{LockId: "x", OwnerId: "o", Version: 1}))

	iter, err := rs.List(ctx)
	require.NoError(t, err)
	require.True(t, iter.Next())
	require.False(t, iter.Next())
}

// TestListResponseShape 直接看 HTTP 层的 truncated 字段。
func TestListResponseShape(t *testing.T) {
	srv := listLimitServer(t, 2)
	rs, err := client.New(context.Background(), client.Config{BaseURL: srv.URL})
	require.NoError(t, err)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		id := string(rune('a' + i))
		require.NoError(t, rs.CreateWithVersion(ctx, id, 1,
			&storage.LockInformation{LockId: id, OwnerId: "o", Version: 1}))
	}

	req, _ := http.NewRequest("GET", srv.URL+"/v1/locks", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, true, body["truncated"])
}
