package mcp

import (
	"errors"
	"net"
	"net/url"
	"strings"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
)

var errOriginForbidden = errors.New("origin not allowed")

type originState struct {
	present bool
	allowed bool
}

func precheckOrigin(cfg config.Config, values []string) (originState, error) {
	if len(values) == 0 {
		return originState{}, nil
	}
	if len(values) != 1 {
		return originState{}, errOriginForbidden
	}
	origin := values[0]
	if !validHTTPSOrigin(origin) {
		return originState{}, errOriginForbidden
	}
	if cfg.Dev.Mode && loopbackBinding(cfg.HTTP.Addr) {
		return originState{present: true, allowed: true}, nil
	}
	for _, allowed := range cfg.MCP.AllowOrigins {
		if origin == allowed && validHTTPSOrigin(allowed) {
			return originState{present: true, allowed: true}, nil
		}
	}
	return originState{}, errOriginForbidden
}

func authorizeOrigin(cfg config.Config, state originState, principal auth.Principal, authenticated bool) error {
	if !cfg.Cloud.Mode {
		return nil
	}
	if state.present {
		if state.allowed && authenticated {
			return nil
		}
		return errOriginForbidden
	}
	if !authenticated {
		return errOriginForbidden
	}
	switch principal.Kind {
	case auth.PrincipalBootstrap, auth.PrincipalCloudAPIKey, auth.PrincipalLegacyJWT,
		auth.PrincipalM2MOnboarding, auth.PrincipalM2MOrg:
		return nil
	default:
		return errOriginForbidden
	}
}

func validHTTPSOrigin(raw string) bool {
	if raw == "" || raw == "null" || strings.TrimSpace(raw) != raw || strings.Contains(raw, ",") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.String() == raw
}

func loopbackBinding(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
