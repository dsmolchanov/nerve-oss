package hybrid

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type cancellationTransport func(*http.Request) (*http.Response, error)

func (f cancellationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type cancellationBody struct {
	cancel context.CancelFunc
	ctx    context.Context
}

func (b *cancellationBody) Read([]byte) (int, error) { b.cancel(); return 0, b.ctx.Err() }
func (b *cancellationBody) Close() error             { return nil }

// Cancellation at either network boundary must remain visible to lifecycle
// callers, regardless of which machine operation is in progress.
func TestHybridCancellationSurvivesEveryRequestBoundary(t *testing.T) {
	key := testKey(t)
	for _, action := range []string{"begin", "complete", "poll", "ack", "send", "receipt", "status"} {
		for _, phase := range []string{"token-headers", "token-body", "api-headers", "api-body"} {
			t.Run(action+"/"+phase, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				transport := cancellationTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if strings.HasSuffix(phase, "headers") {
						cancel()
						return nil, ctx.Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &cancellationBody{cancel: cancel, ctx: ctx}, Request: r}, nil
				})
				httpClient := &http.Client{Transport: transport}
				tokens := newTestTokenSource("https://cloud.example.test/token", key)
				tokens.HTTPClient = httpClient
				if strings.HasPrefix(phase, "api") {
					tokens.cached = "test-token"
					tokens.expires = time.Now().Add(time.Hour)
				}
				client := &Client{BaseURL: "https://cloud.example.test", HTTPClient: httpClient, Tokens: tokens}
				err := client.call(ctx, action, map[string]any{}, nil)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				if calls != 1 {
					t.Fatalf("canceled call repeated %d times", calls)
				}
			})
		}
	}
}

func TestHybridCanceledContextDoesNotRequestOrUseCachedToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tokens := &TokenSource{cached: "test-token", expires: time.Now().Add(time.Hour)}
	if _, err := tokens.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached token ignored cancellation: %v", err)
	}
	client := &Client{Tokens: tokens, BaseURL: "https://cloud.example.test", HTTPClient: &http.Client{Transport: cancellationTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("canceled context reached transport")
		return nil, io.EOF
	})}}
	if err := client.call(ctx, "complete", map[string]any{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call=%v", err)
	}
}
