package emailtransport

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestClassifyHTTPStatus(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantPermanent bool
		wantReason    string
	}{
		{"rate limit is transient", http.StatusTooManyRequests, false, "rate_limited"},
		{"500 is transient", http.StatusInternalServerError, false, "server_error"},
		{"503 is transient", http.StatusServiceUnavailable, false, "server_error"},
		{"401 is permanent", http.StatusUnauthorized, true, "unauthorized"},
		{"403 is permanent", http.StatusForbidden, true, "forbidden"},
		{"404 is permanent", http.StatusNotFound, true, "not_found"},
		{"422 invalid recipient is permanent", http.StatusUnprocessableEntity, true, "invalid_recipient"},
		{"400 bad request is permanent", http.StatusBadRequest, true, "bad_request"},
		{"409 conflict is permanent", http.StatusConflict, true, "bad_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			permanent, reason := ClassifyHTTPStatus(tt.status)
			if permanent != tt.wantPermanent {
				t.Errorf("ClassifyHTTPStatus(%d) permanent = %v, want %v", tt.status, permanent, tt.wantPermanent)
			}
			if reason != tt.wantReason {
				t.Errorf("ClassifyHTTPStatus(%d) reason = %q, want %q", tt.status, reason, tt.wantReason)
			}
		})
	}
}

func TestClassifyProviderError_Nil(t *testing.T) {
	if got := ClassifyProviderError(nil); got != nil {
		t.Errorf("ClassifyProviderError(nil) = %v, want nil", got)
	}
}

func TestClassifyProviderError_Unknown(t *testing.T) {
	raw := errors.New("socket hangup")
	pe := ClassifyProviderError(raw)
	if pe == nil {
		t.Fatal("expected non-nil ProviderError")
	}
	// Default for unclassified errors is transient so providers that don't
	// yet return typed errors keep their current retry behavior.
	if pe.Permanent {
		t.Errorf("expected unknown error to be transient, got permanent=true")
	}
	if pe.Reason != "unknown" {
		t.Errorf("expected reason %q, got %q", "unknown", pe.Reason)
	}
	if !errors.Is(pe, raw) {
		t.Errorf("expected wrapped error to be unwrappable to original, got %v", pe)
	}
}

func TestClassifyProviderError_Passthrough(t *testing.T) {
	inner := NewPermanentError(422, "invalid_recipient", errors.New("bad address"))
	got := ClassifyProviderError(inner)
	if got != inner {
		t.Errorf("ClassifyProviderError should pass *ProviderError through unchanged, got %v", got)
	}
}

func TestClassifyProviderError_Wrapped(t *testing.T) {
	// Errors wrapped with fmt.Errorf + %w should still be unwrappable.
	inner := NewPermanentError(422, "invalid_recipient", errors.New("bad address"))
	wrapped := fmt.Errorf("send failed: %w", inner)
	got := ClassifyProviderError(wrapped)
	if got == nil {
		t.Fatal("expected non-nil")
	}
	if !got.Permanent {
		t.Errorf("expected permanent=true after unwrap, got false")
	}
	if got.Reason != "invalid_recipient" {
		t.Errorf("expected reason %q, got %q", "invalid_recipient", got.Reason)
	}
}

func TestProviderError_Error(t *testing.T) {
	cause := errors.New("underlying problem")
	pe := NewPermanentError(401, "unauthorized", cause)
	if pe.Error() != cause.Error() {
		t.Errorf("Error() should prefer cause message, got %q", pe.Error())
	}

	pe2 := &ProviderError{Permanent: true, Reason: "no_cause"}
	if pe2.Error() != "no_cause" {
		t.Errorf("Error() with nil cause should return Reason, got %q", pe2.Error())
	}
}
