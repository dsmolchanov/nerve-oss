package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"neuralmail/internal/auth"
	"neuralmail/internal/store"
)

// OutboundConfigurationError describes a local setup problem before enqueue.
type OutboundConfigurationError struct {
	Code        string `json:"code"`
	Remediation string `json:"remediation"`
}

func (err *OutboundConfigurationError) Error() string { return err.Code }

func (s *Service) outboundConfigError(code, legacy string) error {
	if s.Config.Cloud.Mode {
		return errors.New(legacy)
	}
	return &OutboundConfigurationError{Code: code, Remediation: "docs/SELF_HOSTING.md#outbound-configuration"}
}

func (s *Service) checkOutboundConfiguration(recipient, provider string) error {
	if !s.Config.Security.AllowOutbound && !isLocalDevRecipient(recipient) {
		return s.outboundConfigError("outbound_disabled", "outbound disabled for non-local domains")
	}
	if len(s.Config.Security.OutboundDomainAllowlist) > 0 && !domainAllowed(recipient, s.Config.Security.OutboundDomainAllowlist) {
		return s.outboundConfigError("recipient_domain_not_allowed", "recipient domain not allowlisted")
	}
	if s.Transport == nil {
		return s.outboundConfigError("outbound_transport_unconfigured", "missing transport registry")
	}
	if _, ok := s.Transport.Outbound(provider); !ok {
		return s.outboundConfigError("outbound_transport_unconfigured", fmt.Sprintf("unknown outbound provider: %s", provider))
	}
	if !s.Config.Cloud.Mode && provider == "smtp" && (strings.TrimSpace(s.Config.SMTP.Host) == "" || s.Config.SMTP.Port < 1 || s.Config.SMTP.Port > 65535) {
		return s.outboundConfigError("smtp_unconfigured", "SMTP host and port are required")
	}
	return nil
}

// Keep the outbox row and its visible message in one transaction in OSS too.
func (s *Service) withOutboundStore(ctx context.Context, fn func(context.Context, *store.Store, auth.Principal) (any, error)) (any, error) {
	if s.Config.Cloud.Mode {
		return s.withScopedStore(ctx, fn)
	}
	var result any
	err := s.Store.RunInTx(ctx, func(st *store.Store) error {
		var err error
		result, err = fn(ctx, st, auth.Principal{})
		return err
	})
	return result, err
}
