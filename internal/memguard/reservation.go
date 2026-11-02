package memguard

import (
	"context"
	"errors"
	"fmt"
)

// Reservation grows one charge without retaining a release callback per read.
// The parent budget mutex protects both total usage and reservation lifecycle.
type Reservation struct {
	budget *Budget
	bytes  int64
	closed bool
}

func (b *Budget) Reserve(ctx context.Context, bytes int64) (*Reservation, error) {
	r := &Reservation{budget: b}
	if err := r.Grow(ctx, bytes); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reservation) Grow(ctx context.Context, bytes int64) error {
	if r == nil || r.budget == nil {
		return errors.New("memory reservation has no budget")
	}
	if bytes < 0 {
		return errors.New("memory reservation must not be negative")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.closed {
		return errors.New("memory reservation is closed")
	}
	if bytes > b.limit-b.used {
		return fmt.Errorf("%w: requested=%d available=%d", ErrExhausted, bytes, b.limit-b.used)
	}
	b.used += bytes
	r.bytes += bytes
	return nil
}

func (r *Reservation) Release() {
	if r == nil || r.budget == nil {
		return
	}
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if !r.closed {
		b.used -= r.bytes
		r.closed = true
	}
}
