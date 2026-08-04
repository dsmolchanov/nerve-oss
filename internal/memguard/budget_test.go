package memguard

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestBudgetAcquireReleaseAndExhaustion(t *testing.T) {
	t.Parallel()

	budget, err := New(10)
	if err != nil {
		t.Fatalf("new budget: %v", err)
	}
	release, err := budget.Acquire(context.Background(), 7)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if budget.Used() != 7 || budget.Available() != 3 {
		t.Fatalf("unexpected accounting: used=%d available=%d", budget.Used(), budget.Available())
	}
	if _, err := budget.Acquire(context.Background(), 4); !errors.Is(err, ErrExhausted) {
		t.Fatalf("expected exhausted error, got %v", err)
	}
	release()
	release()
	if budget.Used() != 0 {
		t.Fatalf("idempotent release leaked bytes: %d", budget.Used())
	}
}

func TestBudgetConcurrentReservationsNeverExceedLimit(t *testing.T) {
	t.Parallel()

	budget, err := New(10)
	if err != nil {
		t.Fatalf("new budget: %v", err)
	}
	start := make(chan struct{})
	releases := make(chan func(), 20)
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			release, err := budget.Acquire(context.Background(), 1)
			if err == nil {
				releases <- release
			}
		}()
	}
	close(start)
	wait.Wait()
	close(releases)

	if got := len(releases); got != 10 {
		t.Fatalf("expected exactly 10 reservations, got %d", got)
	}
	if budget.Used() != budget.Limit() {
		t.Fatalf("budget exceeded or undercounted: used=%d limit=%d", budget.Used(), budget.Limit())
	}
	for release := range releases {
		release()
	}
	if budget.Used() != 0 {
		t.Fatalf("concurrent releases leaked bytes: %d", budget.Used())
	}
}

func TestBudgetHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	budget, err := New(1)
	if err != nil {
		t.Fatalf("new budget: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := budget.Acquire(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if budget.Used() != 0 {
		t.Fatalf("canceled acquire changed accounting: %d", budget.Used())
	}
}
