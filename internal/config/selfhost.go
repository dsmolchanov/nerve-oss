package config

import (
	"errors"
	"net"
	"strings"
)

type LocalAPIKey struct {
	Token    string   `yaml:"token"`
	InboxIDs []string `yaml:"inbox_ids"`
}

func (cfg Config) ValidateSelfhost() error {
	if cfg.Cloud.Mode {
		return nil
	}
	seen := map[string]bool{}
	if cfg.Security.APIKey != "" {
		if strings.TrimSpace(cfg.Security.APIKey) != cfg.Security.APIKey || strings.ContainsAny(cfg.Security.APIKey, " \t\r\n") {
			return errors.New("invalid local API key")
		}
		seen[cfg.Security.APIKey] = true
	}
	for _, key := range cfg.Security.LocalAPIKeys {
		if key.Token == "" || strings.ContainsAny(key.Token, " \t\r\n") || seen[key.Token] || len(key.InboxIDs) == 0 {
			return errors.New("local API keys require unique nonempty tokens and inbox_ids")
		}
		seen[key.Token] = true
		for _, id := range key.InboxIDs {
			if strings.TrimSpace(id) == "" {
				return errors.New("local API key has empty inbox id")
			}
		}
	}
	host, _, err := net.SplitHostPort(cfg.HTTP.Addr)
	if err != nil {
		return errors.New("invalid HTTP bind address")
	}
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if !loopback && len(seen) == 0 && !cfg.Security.AllowUnauthenticated {
		return errors.New("non-loopback OSS bind requires an API key or NERVE_ALLOW_UNAUTHENTICATED=true")
	}
	return nil
}
