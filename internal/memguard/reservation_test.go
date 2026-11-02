package memguard

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestGrowingReservationSharesBudgetAndCannotReopen(t *testing.T) {
	b, _ := New(10)
	r, err := b.Reserve(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	releaseOther, err := b.Acquire(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Grow(context.Background(), 3); err != nil || b.Used() != 10 {
		t.Fatalf("growth=%v used=%d", err, b.Used())
	}
	if err = r.Grow(context.Background(), 1); !errors.Is(err, ErrExhausted) || b.Used() != 10 {
		t.Fatal("growth escaped aggregate budget")
	}
	r.Release()
	r.Release()
	if b.Used() != 4 {
		t.Fatal("release damaged another owner's charge")
	}
	if err = r.Grow(context.Background(), 1); err == nil || b.Used() != 4 {
		t.Fatal("closed reservation reopened")
	}
	releaseOther()
	if b.Used() != 0 {
		t.Fatal("reservation leaked")
	}
}

func TestGrowingReservationFailuresLeaveUsageUnchanged(t *testing.T) {
	b, _ := New(5)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, item := range []struct {
		ctx   context.Context
		bytes int64
	}{{context.Background(), -1}, {context.Background(), 6}, {ctx, 1}} {
		if r, err := b.Reserve(item.ctx, item.bytes); err == nil || r != nil || b.Used() != 0 {
			t.Fatal("failed reservation acquired authority")
		}
	}
	r, err := b.Reserve(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Release()
	if err = r.Grow(ctx, 1); !errors.Is(err, context.Canceled) || b.Used() != 2 {
		t.Fatal("canceled growth changed usage")
	}
	if err = r.Grow(context.Background(), -1); err == nil || b.Used() != 2 {
		t.Fatal("negative growth changed usage")
	}
}

func TestGrowingReservationConcurrentCloseDoesNotLeakOrRevive(t *testing.T) {
	for i := 0; i < 30; i++ {
		b, _ := New(100)
		r, _ := b.Reserve(context.Background(), 0)
		var wg sync.WaitGroup
		for j := 0; j < 20; j++ {
			wg.Add(1)
			go func() { defer wg.Done(); _ = r.Grow(context.Background(), 1) }()
		}
		wg.Add(1)
		go func() { defer wg.Done(); r.Release() }()
		wg.Wait()
		if b.Used() != 0 {
			t.Fatal("concurrent close left a charge")
		}
		if err := r.Grow(context.Background(), 1); err == nil || b.Used() != 0 {
			t.Fatal("concurrent close revived")
		}
	}
}
