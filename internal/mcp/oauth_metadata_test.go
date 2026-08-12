package mcp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProtectedResourceMetadataEndpoints(t *testing.T) {
	wantBody := []byte(`{"resource":"https://nerve-runtime.fly.dev/mcp","authorization_servers":["https://auth.nerve.email"],"scopes_supported":["nerve:onboarding","nerve:email.read","nerve:email.search","nerve:email.draft","nerve:email.reply","nerve:email.compose","nerve:billing.subscribe"],"bearer_methods_supported":["header"]}` + "\n")
	mux := http.NewServeMux()
	handler := ProtectedResourceMetadataHandler()
	mux.Handle(ProtectedResourceMetadataPath, handler)
	mux.Handle(ProtectedResourceMetadataMCPPath, handler)

	var etag string
	for _, path := range []string{ProtectedResourceMetadataPath, ProtectedResourceMetadataMCPPath} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		if !bytes.Equal(recorder.Body.Bytes(), wantBody) {
			t.Fatalf("%s returned non-canonical metadata", path)
		}
		if recorder.Header().Get("Content-Type") != "application/json" || recorder.Header().Get("Cache-Control") != protectedResourceCacheControl {
			t.Fatalf("%s returned wrong representation headers: %v", path, recorder.Header())
		}
		if etag == "" {
			etag = recorder.Header().Get("ETag")
		}
		if etag == "" || recorder.Header().Get("ETag") != etag {
			t.Fatalf("%s returned unstable ETag", path)
		}
	}

	request := httptest.NewRequest(http.MethodGet, ProtectedResourceMetadataMCPPath, nil)
	request.Header.Set("If-None-Match", etag)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotModified || recorder.Body.Len() != 0 {
		t.Fatalf("conditional metadata status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
