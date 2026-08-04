package emailtransport

import (
	"errors"
	"net/http"
)

// ProviderError is the canonical shape returned by outbound adapters when
// send fails with a classifiable reason. The outbox worker uses the
// Permanent flag to decide whether to consume retry budget — permanent
// errors terminate the message immediately via MarkOutboxMessageFailed
// instead of going through exponential backoff.
type ProviderError struct {
	// Permanent indicates the error cannot be recovered by retrying
	// (e.g. invalid recipient, authentication failure, forbidden).
	Permanent bool

	// StatusCode is the provider HTTP status code when the error came
	// from a REST API. Zero for transport errors.
	StatusCode int

	// Reason is a short stable token suitable for metric labels and
	// structured log fields. Examples: "invalid_recipient",
	// "rate_limited", "server_error", "network_error", "bad_request".
	Reason string

	// Cause is the underlying error, preserved for %v formatting and
	// errors.Is / errors.As unwrapping.
	Cause error
}

// Error implements the error interface. Prefers the underlying cause's
// message when present so logs surface the original provider detail.
func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Reason
}

// Unwrap allows errors.Is / errors.As to traverse to the cause.
func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewPermanentError builds a permanent ProviderError. Use this when the
// provider has signalled that retrying will not help (invalid recipient,
// bad auth, malformed request).
func NewPermanentError(statusCode int, reason string, cause error) *ProviderError {
	return &ProviderError{Permanent: true, StatusCode: statusCode, Reason: reason, Cause: cause}
}

// NewTransientError builds a transient (retryable) ProviderError. Use
// this for rate limits, server errors, and network failures.
func NewTransientError(statusCode int, reason string, cause error) *ProviderError {
	return &ProviderError{Permanent: false, StatusCode: statusCode, Reason: reason, Cause: cause}
}

// ClassifyProviderError unwraps err and returns a ProviderError view of
// it. If err is nil, returns nil. If err already carries a *ProviderError
// anywhere in its chain, returns that. Otherwise wraps it as a transient
// error with reason "unknown" — default behavior is to retry on
// unclassified failures so providers that don't yet return typed errors
// keep their current retry semantics.
func ClassifyProviderError(err error) *ProviderError {
	if err == nil {
		return nil
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	return &ProviderError{
		Permanent: false,
		Reason:    "unknown",
		Cause:     err,
	}
}

// ClassifyHTTPStatus maps a provider REST status code into a permanent/
// transient decision with a reason token. Useful for providers that share
// HTTP semantics (Resend, Mailgun, Postmark).
//
// Rate limits (429) and 5xx responses are transient. 401/403/404/422 and
// other 4xx responses are permanent — they will not resolve on retry.
func ClassifyHTTPStatus(status int) (permanent bool, reason string) {
	switch {
	case status == http.StatusTooManyRequests:
		return false, "rate_limited"
	case status >= 500 && status < 600:
		return false, "server_error"
	case status == http.StatusUnauthorized:
		return true, "unauthorized"
	case status == http.StatusForbidden:
		return true, "forbidden"
	case status == http.StatusNotFound:
		return true, "not_found"
	case status == http.StatusUnprocessableEntity:
		return true, "invalid_recipient"
	case status >= 400 && status < 500:
		return true, "bad_request"
	default:
		// 2xx should not arrive here, but treat as not-retryable noise.
		return false, "unknown"
	}
}
