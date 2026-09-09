package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

type serveLoop struct {
	name string
	run  func(context.Context) error
}

// runServeLoops stops sibling loops on signal or failure and joins every loop.
func runServeLoops(ctx context.Context, loops []serveLoop) error {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(loops))
	for _, loop := range loops {
		go func(loop serveLoop) {
			err := loop.run(child)
			if child.Err() != nil && (err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed)) {
				results <- nil
				return
			}
			if err == nil {
				err = errors.New("loop exited unexpectedly")
			}
			results <- fmt.Errorf("%s: %w", loop.name, err)
		}(loop)
	}
	remaining := len(loops)
	var first error
	if remaining > 0 {
		select {
		case first = <-results:
			remaining--
		case <-ctx.Done():
		}
	}
	cancel()
	for ; remaining > 0; remaining-- {
		if err := <-results; first == nil && err != nil {
			first = err
		}
	}
	return first
}
