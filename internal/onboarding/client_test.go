package onboarding

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"neuralmail/internal/auth"
	"neuralmail/internal/mcp"
)

func TestClientSignsFixedDestinationAndAuthenticatedGeneration(t *testing.T) {
	now := time.Unix(1_723_000_000, 0).UTC()
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != operationPaths["start"] || request.Method != http.MethodPost {
			t.Fatalf("request target=%s %s", request.Method, request.URL.Path)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		capturedBody = body
		if got := request.Header.Get("Authorization"); got != "Bearer original-token" {
			t.Fatalf("authorization=%q", got)
		}
		bodyHash := sha256.Sum256(body)
		bodyHashHex := hex.EncodeToString(bodyHash[:])
		if got := request.Header.Get(delegationBodyHashHeader); got != bodyHashHex {
			t.Fatalf("body hash=%q want=%q", got, bodyHashHex)
		}
		canonical := strings.Join([]string{
			"runtime-current", "nonce-1", "1723000000", http.MethodPost,
			operationPaths["start"], bodyHashHex,
		}, "\n")
		mac := hmac.New(sha256.New, []byte("delegation-secret"))
		_, _ = mac.Write([]byte(canonical))
		if got, want := request.Header.Get(delegationSignatureHeader), hex.EncodeToString(mac.Sum(nil)); got != want {
			t.Fatalf("signature=%q want=%q", got, want)
		}
		if request.Header.Get(delegationKeyIDHeader) != "runtime-current" ||
			request.Header.Get(delegationNonceHeader) != "nonce-1" ||
			request.Header.Get(delegationTimestampHeader) != "1723000000" {
			t.Fatalf("delegation headers=%v", request.Header)
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(writer).Encode(map[string]any{"result": map[string]any{
			"resultType": "complete", "onboarding_id": "11111111-1111-4111-8111-111111111111", "generation": 7, "state": "provisioning",
			"mode": "managed_mailbox", "reauthorize": false,
		}})
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, server.Client(), now)
	result, err := client.Start(context.Background(), testCaller(), mcp.OnboardingStartInput{
		IdempotencyKey: "start-1", OrganizationName: "Example", MailboxMode: mcp.OnboardingMailboxManaged,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if result.ResultType != "complete" || result.OnboardingID != "11111111-1111-4111-8111-111111111111" || result.Generation != 7 || result.State != "provisioning" {
		t.Fatalf("result=%+v", result)
	}
	var envelope struct {
		Principal delegationPrincipal      `json:"principal"`
		Input     mcp.OnboardingStartInput `json:"input"`
	}
	if err := json.Unmarshal(capturedBody, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Principal.Kind != auth.PrincipalM2MOnboarding || envelope.Principal.ClientID != "client-1" ||
		envelope.Principal.Generation != 7 || envelope.Principal.TokenID != "token-1" {
		t.Fatalf("delegated principal=%+v", envelope.Principal)
	}
	if envelope.Input.IdempotencyKey != "start-1" || envelope.Input.OrganizationName != "Example" {
		t.Fatalf("delegated input=%+v", envelope.Input)
	}
}

func TestClientRejectsRedirectsAndUnboundedResponses(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", target.URL)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := newTestClient(t, redirect.URL, redirect.Client(), time.Unix(1_723_000_000, 0))
	if _, err := client.Status(context.Background(), testCaller()); err == nil {
		t.Fatal("redirect response was accepted")
	}
	if redirected.Load() {
		t.Fatal("delegation client followed redirect")
	}

	large := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"result":"` + strings.Repeat("x", maxDelegationBodyBytes) + `"}`))
	}))
	defer large.Close()
	client = newTestClient(t, large.URL, large.Client(), time.Unix(1_723_000_000, 0))
	if _, err := client.Status(context.Background(), testCaller()); err == nil || err.Error() != "onboarding delegation protocol error" {
		t.Fatalf("oversized response error=%v", err)
	}
}

func TestClientResponseBodyTimeoutReturnsOutcomeUnknownForEveryOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"result":`))
		writer.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = writer.Write([]byte(`{}}`))
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		BaseURL: server.URL, KeyID: "runtime-current", Secret: "delegation-secret",
		Timeout: 20 * time.Millisecond, HTTPClient: server.Client(), Nonce: func() string { return "nonce-1" },
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	operations := map[string]func() error{
		"start": func() error {
			_, err := client.Start(context.Background(), testCaller(), mcp.OnboardingStartInput{
				IdempotencyKey: "start-1", OrganizationName: "Example", MailboxMode: mcp.OnboardingMailboxManaged,
			})
			return err
		},
		"status": func() error {
			_, err := client.Status(context.Background(), testCaller())
			return err
		},
		"verify domain": func() error {
			_, err := client.VerifyDomain(context.Background(), testCaller())
			return err
		},
		"close": func() error {
			_, err := client.Close(context.Background(), testCaller(), mcp.OnboardingCloseInput{
				IdempotencyKey: "close-1", ExpectedGeneration: 7,
			})
			return err
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, mcp.ErrOnboardingOutcomeUnknown) {
				t.Fatalf("body timeout error=%v", err)
			}
		})
	}
}

func TestClientTransportDisconnectReturnsOutcomeUnknownForEveryOperation(t *testing.T) {
	for _, failure := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "disconnect before headers",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				connection, _, err := writer.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				_ = connection.Close()
			},
		},
		{
			name: "disconnect after partial body",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Content-Length", "100")
				writer.WriteHeader(http.StatusOK)
				_, _ = writer.Write([]byte(`{"result":`))
			},
		},
	} {
		t.Run(failure.name, func(t *testing.T) {
			server := httptest.NewServer(failure.handler)
			defer server.Close()
			client := newTestClient(t, server.URL, server.Client(), time.Unix(1_723_000_000, 0))
			for name, operation := range clientOperations(client) {
				t.Run(name, func(t *testing.T) {
					if err := operation(); !errors.Is(err, mcp.ErrOnboardingOutcomeUnknown) {
						t.Fatalf("disconnect error=%v", err)
					}
				})
			}
		})
	}
}

func TestClientInvalidPostCommitResponseReturnsOutcomeUnknownForEveryMutation(t *testing.T) {
	for _, failure := range []struct {
		name  string
		write func(http.ResponseWriter)
	}{
		{
			name: "proxy 5xx without durable result",
			write: func(writer http.ResponseWriter) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusBadGateway)
				_, _ = writer.Write([]byte(`{"proxy":"failed"}`))
			},
		},
		{
			name: "oversized body",
			write: func(writer http.ResponseWriter) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"result":"` + strings.Repeat("x", maxDelegationBodyBytes) + `"}`))
			},
		},
		{
			name: "unexpected content type",
			write: func(writer http.ResponseWriter) {
				writer.Header().Set("Content-Type", "text/plain")
				_, _ = writer.Write([]byte("upstream failed"))
			},
		},
		{
			name: "malformed JSON",
			write: func(writer http.ResponseWriter) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"result":`))
			},
		},
	} {
		t.Run(failure.name, func(t *testing.T) {
			var committed atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				committed.Add(1)
				failure.write(writer)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, server.Client(), time.Unix(1_723_000_000, 0))
			for name, operation := range mutatingClientOperations(client) {
				t.Run(name, func(t *testing.T) {
					before := committed.Load()
					if err := operation(); !errors.Is(err, mcp.ErrOnboardingOutcomeUnknown) {
						t.Fatalf("post-commit protocol error=%v", err)
					}
					if committed.Load() != before+1 {
						t.Fatalf("handler commit count=%d want=%d", committed.Load(), before+1)
					}
				})
			}
		})
	}
}

func TestClientRejectsSemanticallyInvalidEnvelopesForEveryOperation(t *testing.T) {
	validResult := func() map[string]any {
		return map[string]any{
			"resultType": "complete", "onboarding_id": "11111111-1111-4111-8111-111111111111",
			"generation": 7, "state": "provisioning", "mode": "managed_mailbox", "reauthorize": false,
		}
	}
	for _, failure := range []struct {
		name       string
		statusCode int
		envelope   func() map[string]any
	}{
		{name: "partial result", statusCode: http.StatusOK, envelope: func() map[string]any { return map[string]any{"result": map[string]any{}} }},
		{name: "contradictory result and error", statusCode: http.StatusConflict, envelope: func() map[string]any {
			return map[string]any{"result": validResult(), "error": map[string]any{"code": "conflict", "retryable": false}}
		}},
		{name: "wrong generation", statusCode: http.StatusOK, envelope: func() map[string]any {
			result := validResult()
			result["generation"] = 8
			return map[string]any{"result": result}
		}},
		{name: "invalid state", statusCode: http.StatusOK, envelope: func() map[string]any {
			result := validResult()
			result["state"] = "failed"
			return map[string]any{"result": result}
		}},
		{name: "invalid mode", statusCode: http.StatusOK, envelope: func() map[string]any {
			result := validResult()
			result["mode"] = "mailbox_pool"
			return map[string]any{"result": result}
		}},
		{name: "nil onboarding UUID", statusCode: http.StatusOK, envelope: func() map[string]any {
			result := validResult()
			result["onboarding_id"] = "00000000-0000-0000-0000-000000000000"
			return map[string]any{"result": result}
		}},
		{name: "result with error HTTP status", statusCode: http.StatusBadGateway, envelope: func() map[string]any {
			return map[string]any{"result": validResult()}
		}},
		{name: "error with success HTTP status", statusCode: http.StatusOK, envelope: func() map[string]any {
			return map[string]any{"error": map[string]any{"code": "conflict", "retryable": false}}
		}},
	} {
		t.Run(failure.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(failure.statusCode)
				_ = json.NewEncoder(writer).Encode(failure.envelope())
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, server.Client(), time.Unix(1_723_000_000, 0))
			for name, operation := range clientOperations(client) {
				t.Run(name, func(t *testing.T) {
					err := operation()
					if name == "status" {
						if err == nil || errors.Is(err, mcp.ErrOnboardingOutcomeUnknown) {
							t.Fatalf("status protocol error=%v", err)
						}
						return
					}
					if !errors.Is(err, mcp.ErrOnboardingOutcomeUnknown) {
						t.Fatalf("mutation protocol error=%v", err)
					}
				})
			}
		})
	}
}

func TestClientRejectsCaseFoldedResponseAliasesForEveryOperation(t *testing.T) {
	valid := `{"resultType":"complete","onboarding_id":"11111111-1111-4111-8111-111111111111","generation":7,"state":"active","mode":"managed_mailbox","reauthorize":false}`
	for _, response := range []string{
		`{"Result":` + valid + `}`,
		`{"result":` + valid + `,"Result":` + valid + `}`,
		`{"result":{"resultType":"complete","onboarding_id":"11111111-1111-4111-8111-111111111111","generation":7,"state":"active","mode":"managed_mailbox","reauthorize":false,"DNS_Records":[]}}`,
		`{"error":{"code":"conflict","retryable":false,"Retryable":true}}`,
	} {
		assertProtocolFailureForEveryOperation(t, http.StatusOK, response)
	}
}

func TestClientRequiresCompleteResponseDTOForEveryOperation(t *testing.T) {
	for _, failure := range []struct {
		status int
		body   string
	}{
		{status: http.StatusOK, body: `{"result":{"resultType":"complete","onboarding_id":"11111111-1111-4111-8111-111111111111","generation":7,"state":"active","mode":"managed_mailbox"}}`},
		{status: http.StatusOK, body: `{"result":{"resultType":"complete","onboarding_id":"11111111-1111-4111-8111-111111111111","generation":7,"state":"active","mode":"managed_mailbox","reauthorize":false,"dns_records":[{}]}}`},
		{status: http.StatusOK, body: `{"result":{"resultType":"complete","onboarding_id":"11111111-1111-4111-8111-111111111111","generation":7,"state":"active","mode":"managed_mailbox","reauthorize":false,"dns_checks":[{}]}}`},
		{status: http.StatusOK, body: `{"result":{"resultType":"complete","onboarding_id":"11111111-1111-4111-8111-111111111111","generation":7,"state":"active","mode":"managed_mailbox","reauthorize":false,"dns_records":[{"type":"MX","name":"example.com","value":"mx.example.com","priority":65536}]}}`},
		{status: http.StatusOK, body: `{"result":{"resultType":"complete","onboarding_id":"11111111-1111-4111-8111-111111111111","generation":7,"state":"active","mode":"managed_mailbox","reauthorize":false,"dns_records":null}}`},
		{status: http.StatusConflict, body: `{"error":{"code":"conflict"}}`},
	} {
		assertProtocolFailureForEveryOperation(t, failure.status, failure.body)
	}
}

func TestClientRedactsProtocolParserDetailsForEveryOperation(t *testing.T) {
	const secret = "SYNTHETIC_PROVIDER_SECRET"
	for _, response := range []string{
		`{"error":{"code":"retry","retryable":true,"retry_at":"` + secret + `"}}`,
		`{"error":{"code":"sk_live_` + secret + `","retryable":false}}`,
		`{"result":{"resultType":"complete","onboarding_id":"11111111-1111-4111-8111-111111111111","generation":7,"state":"active","mode":"managed_mailbox","reauthorize":false,"` + secret + `":true}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(response))
		}))
		client := newTestClient(t, server.URL, server.Client(), time.Unix(1_723_000_000, 0))
		for name, operation := range clientOperations(client) {
			t.Run(name, func(t *testing.T) {
				err := operation()
				if err == nil || strings.Contains(err.Error(), secret) {
					t.Fatalf("unredacted protocol error=%v", err)
				}
			})
		}
		server.Close()
	}
}

func TestClientRejectsInvalidAuthorityAndDestinations(t *testing.T) {
	validClient := newTestClient(t, "https://control.internal.example", nil, time.Unix(1_723_000_000, 0))
	invalidCallers := []mcp.OnboardingCaller{
		{},
		{Principal: auth.Principal{Kind: auth.PrincipalM2MOrg, ClientID: "client-1", Generation: 7}, Authorization: "Bearer token"},
		{Principal: auth.Principal{Kind: auth.PrincipalM2MOnboarding, ClientID: "client-1", Generation: 7}, Authorization: "Basic token"},
	}
	for _, caller := range invalidCallers {
		if _, err := validClient.Status(context.Background(), caller); err == nil {
			t.Fatalf("invalid caller accepted: %+v", caller)
		}
	}

	for _, baseURL := range []string{
		"", "http://control.internal.example", "https://user@control.internal.example",
		"https://control.internal.example/path", "https://control.internal.example?target=other",
	} {
		if _, err := NewClient(ClientConfig{BaseURL: baseURL, KeyID: "key", Secret: "secret"}); err == nil {
			t.Fatalf("invalid base URL accepted: %q", baseURL)
		}
	}
}

func TestClientReturnsTypedBusinessError(t *testing.T) {
	retryAt := time.Unix(1_723_000_100, 0).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{
			"code": "onboarding_idempotency_conflict", "retryable": false, "retry_at": retryAt,
		}})
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, server.Client(), time.Unix(1_723_000_000, 0))
	_, err := client.Status(context.Background(), testCaller())
	var businessError *mcp.OnboardingBusinessError
	if !errors.As(err, &businessError) || businessError.Code != "onboarding_idempotency_conflict" || businessError.Retryable || businessError.RetryAt == nil || !businessError.RetryAt.Equal(retryAt) {
		t.Fatalf("business error=%#v err=%v", businessError, err)
	}
}

func TestDecodeDelegationResponseRejectsUnknownAndDuplicateFields(t *testing.T) {
	for _, body := range []string{
		`{"result":{"resultType":"complete","onboarding_id":"11111111-1111-4111-8111-111111111111","generation":7,"state":"active","mode":"managed_mailbox","reauthorize":false,"internal_owner_id":"secret"}}`,
		`{"error":{"code":"conflict","retryable":false,"code":"other"}}`,
	} {
		var decoded delegationResponse
		if err := decodeDelegationResponse([]byte(body), &decoded); err == nil {
			t.Fatalf("ambiguous response accepted: %s", body)
		}
	}
}

func assertProtocolFailureForEveryOperation(t *testing.T, status int, response string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(response))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, server.Client(), time.Unix(1_723_000_000, 0))
	for name, operation := range clientOperations(client) {
		t.Run(name, func(t *testing.T) {
			err := operation()
			if name == "status" {
				if err == nil || errors.Is(err, mcp.ErrOnboardingOutcomeUnknown) || err.Error() != "onboarding delegation protocol error" {
					t.Fatalf("status protocol error=%v", err)
				}
				return
			}
			if !errors.Is(err, mcp.ErrOnboardingOutcomeUnknown) || strings.Contains(err.Error(), response) {
				t.Fatalf("mutation protocol error=%v", err)
			}
		})
	}
}

func newTestClient(t *testing.T, baseURL string, httpClient *http.Client, now time.Time) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{
		BaseURL: baseURL, KeyID: "runtime-current", Secret: "delegation-secret",
		Timeout: time.Second, HTTPClient: httpClient, Now: func() time.Time { return now },
		Nonce: func() string { return "nonce-1" },
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

func testCaller() mcp.OnboardingCaller {
	return mcp.OnboardingCaller{
		Principal: auth.Principal{
			Kind: auth.PrincipalM2MOnboarding, ClientID: "client-1", Generation: 7, TokenID: "token-1",
		},
		Authorization: "Bearer original-token",
	}
}

func clientOperations(client *Client) map[string]func() error {
	return map[string]func() error{
		"start": func() error {
			_, err := client.Start(context.Background(), testCaller(), mcp.OnboardingStartInput{
				IdempotencyKey: "start-1", OrganizationName: "Example", MailboxMode: mcp.OnboardingMailboxManaged,
			})
			return err
		},
		"status": func() error {
			_, err := client.Status(context.Background(), testCaller())
			return err
		},
		"verify domain": func() error {
			_, err := client.VerifyDomain(context.Background(), testCaller())
			return err
		},
		"close": func() error {
			_, err := client.Close(context.Background(), testCaller(), mcp.OnboardingCloseInput{
				IdempotencyKey: "close-1", ExpectedGeneration: 7,
			})
			return err
		},
	}
}

func mutatingClientOperations(client *Client) map[string]func() error {
	return map[string]func() error{
		"start": func() error {
			_, err := client.Start(context.Background(), testCaller(), mcp.OnboardingStartInput{
				IdempotencyKey: "start-1", OrganizationName: "Example", MailboxMode: mcp.OnboardingMailboxManaged,
			})
			return err
		},
		"verify domain": func() error {
			_, err := client.VerifyDomain(context.Background(), testCaller())
			return err
		},
		"close": func() error {
			_, err := client.Close(context.Background(), testCaller(), mcp.OnboardingCloseInput{
				IdempotencyKey: "close-1", ExpectedGeneration: 7,
			})
			return err
		},
	}
}
