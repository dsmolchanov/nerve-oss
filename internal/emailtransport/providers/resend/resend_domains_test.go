package resend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResendDomainsClientCRUDPathsAndAuth(t *testing.T) {
	var gotAuth []string
	var gotPaths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		gotPaths = append(gotPaths, r.Method+" "+r.URL.Path)

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
			_, _ = w.Write([]byte(`{"data":{"id":"d_123","name":"example.com","status":"not_started","records":[{"record":"SPF","name":"send","type":"TXT","value":"v=spf1 include:resend.com ~all","ttl":"Auto","status":"not_started"}]}}`))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/domains/d_123":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"d_123","name":"example.com","status":"pending","records":[]}}`))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/domains/d_123/verify":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"d_123","name":"example.com","status":"pending","records":[]}}`))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/domains":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"d_123","name":"example.com","status":"pending","region":"us-east-1"}]}`))
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

	for i, auth := range gotAuth {
		if auth != "Bearer re_test_key" {
			t.Fatalf("request %d expected auth bearer, got %q", i, auth)
		}
	}
	expectedPaths := []string{
		"POST /domains",
		"GET /domains/d_123",
		"POST /domains/d_123/verify",
		"GET /domains",
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

