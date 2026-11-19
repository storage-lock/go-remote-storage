package metrics

import (
	"context"
	"time"

	go_iterator "github.com/golang-infrastructure/go-iterator"
	"github.com/storage-lock/go-storage"
)

// WrapStorage 给任意 Storage 套一层指标记录。
//
// 只记录耗时与结果，不改变任何语义——特别是不能吞掉或改写错误，
// 否则上层锁逻辑会因为"看起来成功了"而失效。
func WrapStorage(inner storage.Storage, reg *Registry) storage.Storage {
	if inner == nil || reg == nil {
		return inner
	}
	return &instrumentedStorage{inner: inner, reg: reg}
}

type instrumentedStorage struct {
	inner storage.Storage
	reg   *Registry
}

func (s *instrumentedStorage) GetName() string                    { return s.inner.GetName() }
func (s *instrumentedStorage) Capabilities() []storage.StorageCapability {
	return s.inner.Capabilities()
}
func (s *instrumentedStorage) Init(ctx context.Context) error {
	return s.observe("init", func() error { return s.inner.Init(ctx) })
}
func (s *instrumentedStorage) Close(ctx context.Context) error {
	return s.observe("close", func() error { return s.inner.Close(ctx) })
}
func (s *instrumentedStorage) GetTime(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.observe("get_time", func() error {
		var e error
		t, e = s.inner.GetTime(ctx)
		return e
	})
	return t, err
}
func (s *instrumentedStorage) Get(ctx context.Context, id string) (string, error) {
	var v string
	err := s.observe("get", func() error {
		var e error
		v, e = s.inner.Get(ctx, id)
		return e
	})
	return v, err
}
func (s *instrumentedStorage) CreateWithVersion(ctx context.Context, id string, ver storage.Version,
	info *storage.LockInformation) error {
	return s.observe("create", func() error { return s.inner.CreateWithVersion(ctx, id, ver, info) })
}
func (s *instrumentedStorage) UpdateWithVersion(ctx context.Context, id string, exp, next storage.Version,
	info *storage.LockInformation) error {
	return s.observe("update", func() error { return s.inner.UpdateWithVersion(ctx, id, exp, next, info) })
}
func (s *instrumentedStorage) DeleteWithVersion(ctx context.Context, id string, exp storage.Version,
	info *storage.LockInformation) error {
	return s.observe("delete", func() error { return s.inner.DeleteWithVersion(ctx, id, exp, info) })
}
func (s *instrumentedStorage) List(ctx context.Context) (go_iterator.Iterator[*storage.LockInformation], error) {
	var it go_iterator.Iterator[*storage.LockInformation]
	err := s.observe("list", func() error {
		var e error
		it, e = s.inner.List(ctx)
		return e
	})
	return it, err
}

// observe 包一层计时。错误原样返回，不做任何改写。
func (s *instrumentedStorage) observe(op string, fn func() error) error {
	start := time.Now()
	err := fn()
	s.reg.ObserveStorage(op, err, time.Since(start))
	return err
}
