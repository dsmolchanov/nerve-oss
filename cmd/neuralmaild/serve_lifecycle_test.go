package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestServeWorkerOptions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cloud bool
		args  []string
		want  bool
		fail  bool
	}{
		{name: "OSS default", want: true}, {name: "cloud default", cloud: true},
		{name: "disable OSS", args: []string{"--with-worker=false"}},
		{name: "enable cloud", cloud: true, args: []string{"--with-worker"}, want: true},
		{name: "invalid", args: []string{"--with-worker=invalid"}, fail: true},
		{name: "unexpected argument", args: []string{"worker"}, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serveWorkerOption(tc.cloud, tc.args)
			if (err != nil) != tc.fail || (!tc.fail && got != tc.want) {
				t.Fatalf("got %t/%v", got, err)
			}
		})
	}
}

func TestServeLoopsJoinOnSignalAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "failure", false: "signal"}[fail], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{}, 2)
			var joined atomic.Int32
			failure := errors.New("worker failed")
			results := make(chan error, 1)
			go func() {
				results <- runServeLoops(ctx, []serveLoop{
					{name: "HTTP", run: func(ctx context.Context) error { started <- struct{}{}; <-ctx.Done(); joined.Add(1); return ctx.Err() }},
					{name: "outbox", run: func(ctx context.Context) error { started <- struct{}{}; <-ctx.Done(); joined.Add(1); return ctx.Err() }},
					{name: "trigger", run: func(ctx context.Context) error {
						<-started
						<-started
						if fail {
							return failure
						}
						cancel()
						return ctx.Err()
					}},
				})
			}()
			err := <-results
			if fail && !errors.Is(err, failure) {
				t.Fatalf("lost failure: %v", err)
			}
			if !fail && err != nil {
				t.Fatal(err)
			}
			if joined.Load() != 2 {
				t.Fatal("returned before sibling loops joined")
			}
		})
	}
}
