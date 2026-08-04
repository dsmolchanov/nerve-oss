// Package memguard enforces an aggregate byte budget for operations that
// materialize request bodies or attachment content in memory.
package memguard

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrExhausted = errors.New("memory budget exhausted")

type Budget struct {
	mu    sync.Mutex
	limit int64
	used  int64
}

func New(limit int64) (*Budget, error) {
	if limit <= 0 {
		return nil, errors.New("memory budget must be positive")
	}
	return &Budget{limit: limit}, nil
}

func (b *Budget) Acquire(ctx context.Context, bytes int64) (func(), error) {
	if b == nil {
		return nil, errors.New("memory budget is nil")
	}
	if bytes < 0 {
		return nil, errors.New("memory reservation must not be negative")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bytes == 0 {
		return func() {}, nil
	}

	b.mu.Lock()
	if bytes > b.limit-b.used {
		available := b.limit - b.used
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: requested=%d available=%d", ErrExhausted, bytes, available)
	}
	b.used += bytes
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.used -= bytes
			b.mu.Unlock()
		})
	}, nil
}

func (b *Budget) Limit() int64 {
	if b == nil {
		return 0
	}
	return b.limit
}

func (b *Budget) Used() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func (b *Budget) Available() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit - b.used
}
