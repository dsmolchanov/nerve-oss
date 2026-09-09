package llm

import "errors"

// ErrUnavailable means no real LLM provider was selected. Retrying requires
// configuration, not a paid subscription or an automatic retry loop.
var ErrUnavailable = errors.New("ai_unavailable")

func RequireAvailable(provider Provider) error {
	if provider == nil || provider.Name() == "noop" {
		return ErrUnavailable
	}
	return nil
}
