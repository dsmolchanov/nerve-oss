package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"neuralmail/internal/config"
	hybridtransport "neuralmail/internal/emailtransport/providers/hybrid"
)

func hybridStatePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "installation.json")
}

func writeHybridState(t *testing.T, path string, mutate func(*hybridtransport.State)) hybridtransport.State {
	t.Helper()
	key, err := hybridtransport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	state := hybridtransport.State{
		Version: hybridtransport.StateVersion, Phase: hybridtransport.PhaseInstalled,
		CloudBaseURL: "https://cloud.example.test", TokenEndpoint: "https://auth.example.test/oauth/token",
		Resource: "https://runtime.example.test/mcp", ClientID: "hybrid-client", Generation: 1,
		OrgID: uuid.NewString(), InstallationID: uuid.NewString(), InboxID: uuid.NewString(),
		AuthorityID: "cloud.example.test", Key: key,
	}
	if mutate != nil {
		mutate(&state)
	}
	if err := (hybridtransport.Store{Path: path}).Save(state); err != nil {
		t.Fatal(err)
	}
	return state
}

// A host with no hybrid configuration must behave exactly as before.
func TestHybridRuntimeStaysOffWhenUnconfigured(t *testing.T) {
	cfg := config.Default()
	runtime, err := newHybridRuntime(cfg)
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%v err=%v, want no hybrid transport", runtime, err)
	}
}

// A configured path with no state yet, or a pairing still awaiting approval,
// is a normal intermediate state during setup. Refusing to start there would
// mean an operator cannot run the very daemon they are configuring.
func TestHybridRuntimeServesWhilePairingIsIncomplete(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T) string{
		"never connected": func(t *testing.T) string { return hybridStatePath(t) },
		"awaiting approval": func(t *testing.T) string {
			path := hybridStatePath(t)
			key, err := hybridtransport.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			state := hybridtransport.State{
				Version: hybridtransport.StateVersion, Phase: hybridtransport.PhaseConnecting,
				CloudBaseURL: "https://cloud.example.test", TokenEndpoint: "https://auth.example.test/oauth/token",
				Resource: "https://runtime.example.test/mcp", ClientID: "hybrid-client", Generation: 1,
				InboxID: uuid.NewString(), AuthorityID: "cloud.example.test", Key: key,
			}
			if err := (hybridtransport.Store{Path: path}).Save(state); err != nil {
				t.Fatal(err)
			}
			return path
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Hybrid.StatePath = prepare(t)
			runtime, err := newHybridRuntime(cfg)
			if err != nil || runtime != nil {
				t.Fatalf("runtime=%v err=%v, want the daemon to serve without hybrid", runtime, err)
			}
		})
	}
}

// Starting with a half-understood identity is worse than not starting: the
// runtime would sign with a key Cloud cannot match, or keep using one that
// must be treated as disclosed.
func TestHybridRuntimeRefusesUnsafeOrUnreadableState(t *testing.T) {
	t.Run("readable by others", func(t *testing.T) {
		cfg := config.Default()
		cfg.Hybrid.StatePath = hybridStatePath(t)
		writeHybridState(t, cfg.Hybrid.StatePath, nil)
		if err := os.Chmod(cfg.Hybrid.StatePath, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := newHybridRuntime(cfg)
		if !errors.Is(err, hybridtransport.ErrStateUnsafePermissions) {
			t.Fatalf("started with an exposed key: %v", err)
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		cfg := config.Default()
		cfg.Hybrid.StatePath = hybridStatePath(t)
		if err := os.WriteFile(cfg.Hybrid.StatePath, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := newHybridRuntime(cfg); err == nil {
			t.Fatal("started with unreadable installation state")
		}
	})
	t.Run("key is not its own thumbprint", func(t *testing.T) {
		cfg := config.Default()
		cfg.Hybrid.StatePath = hybridStatePath(t)
		state := writeHybridState(t, cfg.Hybrid.StatePath, nil)
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Fatal(err)
		}
		generic["key"].(map[string]any)["kid"] = strings.Repeat("A", 43)
		tampered, err := json.Marshal(generic)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.Hybrid.StatePath, tampered, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := newHybridRuntime(cfg); err == nil {
			t.Fatal("started with a key that does not match its recorded id")
		}
	})
}

// A connected runtime contributes both directions under one provider name, so
// the inbox routing that selects "hybrid" reaches the same installation for
// pulling and sending.
func TestHybridRuntimeRegistersBothDirections(t *testing.T) {
	cfg := config.Default()
	cfg.Hybrid.StatePath = hybridStatePath(t)
	state := writeHybridState(t, cfg.Hybrid.StatePath, nil)
	runtime, err := newHybridRuntime(cfg)
	if err != nil || runtime == nil {
		t.Fatalf("runtime=%v err=%v", runtime, err)
	}
	if runtime.Inbound.Name() != hybridtransport.ProviderName || runtime.Outbound.Name() != hybridtransport.ProviderName {
		t.Fatal("adapters do not share the hybrid provider name")
	}
	if runtime.State.InstallationID != state.InstallationID {
		t.Fatal("runtime was built from the wrong installation")
	}
}

// A paired host receives all of its mail by pulling, so it must not inherit a
// JMAP mirror's half-minute gap between checks.
func TestHybridPollIntervalOverridesTheMirrorInterval(t *testing.T) {
	cfg := config.Default()
	plain := &App{Config: cfg}
	if plain.pollInterval() != cfg.JMAP.PollInterval {
		t.Fatalf("unpaired interval %v, want the JMAP interval %v", plain.pollInterval(), cfg.JMAP.PollInterval)
	}
	paired := &App{Config: cfg, Hybrid: &hybridtransport.Runtime{}}
	if paired.pollInterval() != cfg.Hybrid.PollInterval {
		t.Fatalf("paired interval %v, want the hybrid interval %v", paired.pollInterval(), cfg.Hybrid.PollInterval)
	}
	if cfg.Hybrid.PollInterval >= cfg.JMAP.PollInterval {
		t.Fatal("the hybrid default should poll more often than the JMAP mirror")
	}
}
