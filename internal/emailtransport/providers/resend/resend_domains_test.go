package resend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestResendDomainsClientCRUDPathsAndAuth(t *testing.T) {
	var gotAuth []string
	var gotPaths []string
	var gotUserAgents []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		gotPaths = append(gotPaths, r.Method+" "+r.URL.Path)
		gotUserAgents = append(gotUserAgents, r.Header.Get("User-Agent"))

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/domains":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			if body["name"] != "example.com" {
				t.Fatalf("expected name=example.com, got %#v", body["name"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"domain","id":"d_123","name":"example.com","status":"not_started","capabilities":{"sending":"enabled","receiving":"disabled"},"records":[{"record":"SPF","name":"send","type":"TXT","value":"v=spf1 include:resend.com ~all","ttl":"Auto","status":"not_started"}]}`))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/domains/d_123":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"domain","id":"d_123","name":"example.com","status":"pending","records":[]}`))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/domains/d_123/verify":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"domain","id":"d_123","name":"example.com","status":"pending","records":[]}`))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/domains":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"d_123","name":"example.com","status":"pending","region":"us-east-1"}]}`))
			return
		case r.Method == http.MethodPatch && r.URL.Path == "/domains/d_123":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode enable receiving body: %v", err)
			}
			caps, ok := body["capabilities"].(map[string]any)
			if !ok {
				t.Fatalf("expected capabilities map, got %#v", body["capabilities"])
			}
			if caps["receiving"] != "enabled" {
				t.Fatalf("expected receiving=enabled, got %#v", caps["receiving"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"d_123","object":"domain"}`))
			return
		case r.Method == http.MethodDelete && r.URL.Path == "/domains/d_123":
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewDomainsClient(Config{
		APIKey:     "re_test_key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})

	created, err := client.CreateDomain(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if created.ID != "d_123" || created.Name != "example.com" {
		t.Fatalf("unexpected created domain: %+v", created)
	}
	if len(created.Records) != 1 || created.Records[0].Record != "SPF" {
		t.Fatalf("expected SPF record, got %+v", created.Records)
	}
	if created.Capabilities.Sending != "enabled" || created.Capabilities.Receiving != "disabled" {
		t.Fatalf("expected domain capabilities, got %+v", created.Capabilities)
	}

	fetched, err := client.GetDomain(context.Background(), "d_123")
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	if fetched.ID != "d_123" || fetched.Status != "pending" {
		t.Fatalf("unexpected fetched domain: %+v", fetched)
	}

	verified, err := client.VerifyDomain(context.Background(), "d_123")
	if err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	if verified.ID != "d_123" {
		t.Fatalf("unexpected verified domain: %+v", verified)
	}

	listed, err := client.ListDomains(context.Background())
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "d_123" {
		t.Fatalf("unexpected listed domains: %+v", listed)
	}

	if err := client.EnableReceiving(context.Background(), "d_123"); err != nil {
		t.Fatalf("EnableReceiving: %v", err)
	}
	if err := client.DeleteDomain(context.Background(), "d_123"); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}

	for i, auth := range gotAuth {
		if auth != "Bearer re_test_key" {
			t.Fatalf("request %d expected auth bearer, got %q", i, auth)
		}
		if gotUserAgents[i] != "nerve-email/1.0" {
			t.Fatalf("request %d expected repository-neutral user agent, got %q", i, gotUserAgents[i])
		}
	}
	expectedPaths := []string{
		"POST /domains",
		"GET /domains/d_123",
		"POST /domains/d_123/verify",
		"GET /domains",
		"PATCH /domains/d_123",
		"DELETE /domains/d_123",
	}
	if len(gotPaths) != len(expectedPaths) {
		t.Fatalf("expected %d requests, got %d: %+v", len(expectedPaths), len(gotPaths), gotPaths)
	}
	for i := range expectedPaths {
		if gotPaths[i] != expectedPaths[i] {
			t.Fatalf("request %d expected %q got %q", i, expectedPaths[i], gotPaths[i])
		}
	}
}

func TestResendDomainsDeleteTreatsNotFoundAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/domains/already-gone" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewDomainsClient(Config{APIKey: "re_test_key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if err := client.DeleteDomain(context.Background(), "already-gone"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestResendDomainsClientRetriesRateLimitedGet(t *testing.T) {
	attempts := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/domains/d_123" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer re_test_key" {
			t.Fatalf("missing auth header")
		}

		attempts++
		if attempts == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"statusCode":429,"name":"rate_limit_exceeded"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"d_123","name":"example.com","status":"pending","records":[]}}`))
	}))
	defer srv.Close()

	client := NewDomainsClient(Config{
		APIKey:     "re_test_key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})

	got, err := client.GetDomain(context.Background(), "d_123")
	if err != nil {
		t.Fatalf("GetDomain retry failed: %v", err)
	}
	if got.ID != "d_123" {
		t.Fatalf("unexpected domain id: %+v", got)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestResendDomainsClientNeverRetriesAmbiguousCreate(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/domains" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				attempts++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"name":"provider_unavailable","message":"tenant-controlled detail"}`))
			}))
			defer srv.Close()

			client := NewDomainsClient(Config{APIKey: "re_test_key", BaseURL: srv.URL, HTTPClient: srv.Client()})
			_, err := client.CreateDomain(context.Background(), "example.com")
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected typed retryable API error, got %T: %v", err, err)
			}
			if attempts != 1 {
				t.Fatalf("ambiguous non-idempotent create made %d POST attempts, want 1", attempts)
			}
			if !apiErr.Retryable || apiErr.StatusCode != status || apiErr.Code != "provider_unavailable" {
				t.Fatalf("ambiguous create classification=%+v", apiErr)
			}
		})
	}

	t.Run("transport uncertainty", func(t *testing.T) {
		attempts := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/domains" {
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
			attempts++
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("httptest response does not support connection hijacking")
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack ambiguous create response: %v", err)
			}
			_ = connection.Close()
		}))
		defer srv.Close()

		client := NewDomainsClient(Config{APIKey: "re_test_key", BaseURL: srv.URL, HTTPClient: srv.Client()})
		if _, err := client.CreateDomain(context.Background(), "example.com"); err == nil {
			t.Fatal("transport uncertainty unexpectedly returned create success")
		}
		if attempts != 1 {
			t.Fatalf("transport-uncertain create made %d POST attempts, want 1", attempts)
		}
	})
}

func TestResendDomainsMutationsUseAuthoritativeReadback(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch len(requests) {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != "/domains/d_exact/verify" {
				t.Fatalf("unexpected verify request: %s %s", r.Method, r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"object":"domain","id":"d_exact"}`))
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != "/domains/d_exact" {
				t.Fatalf("unexpected verify readback: %s %s", r.Method, r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"id":"d_exact","name":"Example.COM.","status":"pending","capabilities":{"sending":"enabled","receiving":"disabled"},"records":[]}`))
		case 3:
			if r.Method != http.MethodPatch || r.URL.Path != "/domains/d_exact" {
				t.Fatalf("unexpected receiving mutation: %s %s", r.Method, r.URL.Path)
			}
			var body struct {
				Capabilities DomainCapabilities `json:"capabilities"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode receiving mutation: %v", err)
			}
			if body.Capabilities.Receiving != "enabled" {
				t.Fatalf("expected receiving enable, got %+v", body.Capabilities)
			}
			_, _ = w.Write([]byte(`{"object":"domain","id":"d_exact"}`))
		case 4:
			if r.Method != http.MethodGet || r.URL.Path != "/domains/d_exact" {
				t.Fatalf("unexpected receiving readback: %s %s", r.Method, r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"id":"d_exact","name":"example.com","status":"verified","capabilities":{"sending":"enabled","receiving":"enabled"},"records":[]}`))
		default:
			t.Fatalf("unexpected request %d: %s %s", len(requests), r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewDomainsClient(Config{APIKey: "re_test_key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	verified, err := client.VerifyDomain(context.Background(), "d_exact")
	if err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	if verified.Name != "Example.COM." || verified.Status != "pending" {
		t.Fatalf("verify did not return authoritative state: %+v", verified)
	}
	enabled, err := client.SetReceiving(context.Background(), "d_exact", true)
	if err != nil {
		t.Fatalf("SetReceiving: %v", err)
	}
	if enabled.Status != "verified" || enabled.Capabilities.Receiving != "enabled" {
		t.Fatalf("receiving did not return authoritative state: %+v", enabled)
	}
	if len(requests) != 4 {
		t.Fatalf("expected two mutation/readback pairs, got %+v", requests)
	}
}

func TestResendDomainsCanonicalLookupIsBoundedAndDoesNotSelectAWinner(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/domains" {
			t.Fatalf("unexpected list request: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "100" {
			t.Fatalf("expected bounded page size, got %q", r.URL.Query().Get("limit"))
		}
		switch requests {
		case 1:
			if r.URL.Query().Get("after") != "" {
				t.Fatalf("first page unexpectedly had cursor")
			}
			_, _ = w.Write([]byte(`{"object":"list","has_more":true,"data":[{"id":"cursor_1","name":"other.example","status":"verified"}]}`))
		case 2:
			if r.URL.Query().Get("after") != "cursor_1" {
				t.Fatalf("expected cursor_1, got %q", r.URL.Query().Get("after"))
			}
			_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[{"id":"d_one","name":"Example.COM.","status":"pending"},{"id":"d_two","name":"example.com","status":"verified"}]}`))
		default:
			t.Fatalf("lookup exceeded expected page count")
		}
	}))
	defer srv.Close()

	client := NewDomainsClient(Config{APIKey: "re_test_key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	matches, err := client.FindDomainsByCanonicalName(context.Background(), "example.com.")
	if err != nil {
		t.Fatalf("FindDomainsByCanonicalName: %v", err)
	}
	if len(matches) != 2 || matches[0].ID != "d_one" || matches[1].ID != "d_two" {
		t.Fatalf("expected all ambiguous matches without selection, got %+v", matches)
	}
}

func TestResendDomainsCanonicalLookupRejectsCursorCycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","has_more":true,"data":[{"id":"same_cursor","name":"other.example","status":"verified"}]}`))
	}))
	defer srv.Close()

	client := NewDomainsClient(Config{APIKey: "re_test_key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	_, err := client.FindDomainsByCanonicalName(context.Background(), "example.com")
	if !errors.Is(err, ErrDomainPaginationCycle) {
		t.Fatalf("expected bounded cursor-cycle error, got %v", err)
	}
}

func TestResendDomainsCanonicalLookupRejectsUnboundedInventory(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		id := "cursor_" + strconv.Itoa(requests)
		_, _ = w.Write([]byte(`{"object":"list","has_more":true,"data":[{"id":"` + id + `","name":"other.example","status":"verified"}]}`))
	}))
	defer srv.Close()

	client := NewDomainsClient(Config{APIKey: "re_test_key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	_, err := client.FindDomainsByCanonicalName(context.Background(), "example.com")
	if !errors.Is(err, ErrDomainPaginationBound) {
		t.Fatalf("expected bounded-inventory error, got %v", err)
	}
	if requests != maxResendDomainPages {
		t.Fatalf("expected exactly %d bounded pages, got %d", maxResendDomainPages, requests)
	}
}

func TestResendDomainsAPIErrorsAreTypedBoundedAndRedacted(t *testing.T) {
	const recognizableSecret = "recognizable-secret-request-body"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/domains/missing" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"name":"not_found","message":"` + recognizableSecret + `"}`))
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"name":"validation_error","message":"` + recognizableSecret + `"}`))
	}))
	defer srv.Close()

	client := NewDomainsClient(Config{APIKey: "re_test_key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	_, err := client.GetDomain(context.Background(), "invalid")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected typed API error, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusUnprocessableEntity || apiErr.Code != "validation_error" || apiErr.Retryable {
		t.Fatalf("unexpected API classification: %+v", apiErr)
	}
	if strings.Contains(err.Error(), recognizableSecret) {
		t.Fatalf("provider response secret leaked through error: %q", err.Error())
	}
	_, err = client.GetDomain(context.Background(), "missing")
	if !IsNotFound(err) {
		t.Fatalf("expected authoritative not-found classification, got %v", err)
	}
}

func TestResendRetryDelayCapsProviderAdvice(t *testing.T) {
	if got := resendRetryDelay("3600", 0); got != resendRetryMaxDelay {
		t.Fatalf("expected retry advice capped at %s, got %s", resendRetryMaxDelay, got)
	}
}

func TestIsDefinitiveCreateRejectionIsNarrow(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "bad request validation", err: &APIError{Operation: "create_domain", StatusCode: http.StatusBadRequest, Code: "validation_error"}, want: true},
		{name: "unprocessable validation", err: &APIError{Operation: "create_domain", StatusCode: http.StatusUnprocessableEntity, Code: "validation_error"}, want: true},
		{name: "auth", err: &APIError{Operation: "create_domain", StatusCode: http.StatusUnauthorized, Code: "validation_error"}},
		{name: "conflict", err: &APIError{Operation: "create_domain", StatusCode: http.StatusConflict, Code: "validation_error"}},
		{name: "retryable", err: &APIError{Operation: "create_domain", StatusCode: http.StatusUnprocessableEntity, Code: "validation_error", Retryable: true}},
		{name: "wrong operation", err: &APIError{Operation: "get_domain", StatusCode: http.StatusUnprocessableEntity, Code: "validation_error"}},
		{name: "wrong code", err: &APIError{Operation: "create_domain", StatusCode: http.StatusUnprocessableEntity, Code: "invalid_request"}},
		{name: "transport", err: errors.New("transport uncertainty")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsDefinitiveCreateRejection(test.err); got != test.want {
				t.Fatalf("IsDefinitiveCreateRejection(%v)=%t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestBuildDomainQuarantineObservationsIsDeterministicAndFailClosed(t *testing.T) {
	left, err := BuildDomainQuarantineObservations([]Domain{
		{ID: "rd-b", Name: "B.Example.", Status: " Pending "},
		{ID: "rd-a", Name: "a.example", Status: "verified"},
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := BuildDomainQuarantineObservations([]Domain{
		{ID: "rd-a", Name: "A.EXAMPLE.", Status: "VERIFIED"},
		{ID: "rd-b", Name: "b.example", Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || len(right) != 2 || left[0].ProviderDomainID != "rd-a" ||
		left[1].ProviderDomainID != "rd-b" || left[0].InventorySHA256 != left[1].InventorySHA256 ||
		left[0].InventorySHA256 != right[0].InventorySHA256 || len(left[0].InventorySHA256) != 64 {
		t.Fatalf("non-deterministic quarantine observations left=%+v right=%+v", left, right)
	}
	for _, invalid := range [][]Domain{
		nil,
		{{ID: " rd-a", Name: "a.example"}},
		{{ID: "rd/a", Name: "a.example"}},
		{{ID: "rd-a", Name: "a.example"}, {ID: "rd-a", Name: "a.example"}},
		{{ID: "rd-a", Name: "not a domain"}},
	} {
		if got, err := BuildDomainQuarantineObservations(invalid); err == nil || got != nil {
			t.Fatalf("invalid quarantine inventory accepted: got=%+v err=%v", got, err)
		}
	}
}

func TestValidateDomainIDMatchesExactIDOperations(t *testing.T) {
	valid := []string{"rd-a", strings.Repeat("a", maxResendDomainIDBytes)}
	for _, value := range valid {
		if err := ValidateDomainID(value); err != nil {
			t.Fatalf("ValidateDomainID(%q) returned %v", value, err)
		}
	}
	invalid := []string{
		"", " rd-a", "rd-a ", "rd/a", `rd\\a`, "rd?a", "rd#a",
		strings.Repeat("a", maxResendDomainIDBytes+1),
	}
	for _, value := range invalid {
		if err := ValidateDomainID(value); err == nil {
			t.Fatalf("ValidateDomainID(%q) unexpectedly succeeded", value)
		}
	}
}

func TestExactDomainOperationsRejectNonExactIdentityBeforeHTTP(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer srv.Close()
	client := NewDomainsClient(Config{APIKey: "re_test_key", BaseURL: srv.URL, HTTPClient: srv.Client()})

	if _, err := client.GetDomain(context.Background(), " rd-a"); err == nil {
		t.Fatal("GetDomain accepted a non-exact identity")
	}
	if _, err := client.VerifyDomain(context.Background(), "rd-a "); err == nil {
		t.Fatal("VerifyDomain accepted a non-exact identity")
	}
	if _, err := client.SetReceiving(context.Background(), " rd-a ", true); err == nil {
		t.Fatal("SetReceiving accepted a non-exact identity")
	}
	if err := client.DeleteDomain(context.Background(), "rd-a "); err == nil {
		t.Fatal("DeleteDomain accepted a non-exact identity")
	}
	if requests != 0 {
		t.Fatalf("invalid exact identities made %d HTTP requests", requests)
	}
}
