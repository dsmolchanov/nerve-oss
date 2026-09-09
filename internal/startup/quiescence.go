package startup

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// SchemaQuiescence is checked before application construction, so even startup
// writes and background/provider workers are excluded. Existing processes must
// first be stopped by the deployment; this is not a remote kill switch.
func SchemaQuiescence(ctx context.Context, mode string, cloud bool, command, address string) (bool, error) {
	if mode == "" {
		return false, nil
	}
	if mode != "quiescent" || !cloud {
		return true, errors.New("invalid schema transition mode")
	}
	switch command {
	case "worker":
		<-ctx.Done()
		return true, nil
	case "serve":
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return true, err
		}
		return true, serveQuiescence(ctx, listener)
	default:
		return true, errors.New("command is unavailable during schema quiescence")
	}
}

func serveQuiescence(ctx context.Context, listener net.Listener) error {
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler does not parse credentials or provider payloads. All paths,
		// including health checks and webhooks, reject rather than falsely ACK.
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "close")
		http.Error(w, "schema transition in progress", http.StatusServiceUnavailable)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 * 1024}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	err := server.Serve(listener)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
