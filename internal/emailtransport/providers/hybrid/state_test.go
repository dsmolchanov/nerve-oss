package hybrid

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testKey(t *testing.T) Key {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testState(t *testing.T, key Key) State {
	t.Helper()
	return State{
		Version: StateVersion, Phase: PhaseInstalled, CloudBaseURL: "https://cloud.example.test",
		TokenEndpoint: "https://auth.example.test/oauth/token", Resource: "https://runtime.example.test/mcp",
		ClientID: "hybrid-client", Generation: 1, OrgID: uuid.NewString(),
		InstallationID: uuid.NewString(), InboxID: uuid.NewString(),
		AuthorityID: "cloud.example.test", Key: key,
	}
}

func TestHybridStateRoundTripsAndStaysOwnerOnly(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested", "state")
	store := Store{Path: filepath.Join(directory, "installation.json")}
	if _, err := store.Load(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("unpaired runtime: %v", err)
	}
	want := testState(t, testKey(t))
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Key.PrivateKeyPEM != want.Key.PrivateKeyPEM || got.Key.KID != want.Key.KID || got.InstallationID != want.InstallationID {
		t.Fatal("state did not round trip")
	}
	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != stateFileMode {
		t.Fatalf("state mode %04o, want %04o", info.Mode().Perm(), stateFileMode)
	}
	// The directory is created along the way and must not be group readable
	// either: a readable directory is not a readable key, but it is one
	// mistake away.
	parent, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state directory mode %04o is readable beyond its owner", parent.Mode().Perm())
	}
	if err := store.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("after disconnect: %v", err)
	}
	// Remove is idempotent: a second disconnect after a partial one must not
	// fail the command.
	if err := store.Remove(); err != nil {
		t.Fatal(err)
	}
}

// The file holds the only long-lived secret on this host. Loading it when any
// other account can read it would quietly keep using a key that must be treated
// as disclosed, so every loosened mode refuses.
func TestHybridStateRefusesToLoadWhenReadableByOthers(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			store := Store{Path: filepath.Join(t.TempDir(), "installation.json")}
			if err := store.Save(testState(t, testKey(t))); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(store.Path, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(); !errors.Is(err, ErrStateUnsafePermissions) {
				t.Fatalf("mode %04o loaded: %v", mode.Perm(), err)
			}
		})
	}
}

func TestHybridStateRefusesUnreadableOrNewerContent(t *testing.T) {
	key := testKey(t)
	other := testKey(t)
	mutations := map[string]func(*State){
		"empty client id":      func(s *State) { s.ClientID = "" },
		"zero generation":      func(s *State) { s.Generation = 0 },
		"non uuid org":         func(s *State) { s.OrgID = "not-a-uuid" },
		"non uuid inbox":       func(s *State) { s.InboxID = "not-a-uuid" },
		"plaintext cloud":      func(s *State) { s.CloudBaseURL = "http://cloud.example.test" },
		"plaintext token":      func(s *State) { s.TokenEndpoint = "http://auth.example.test/oauth/token" },
		"credentials in url":   func(s *State) { s.CloudBaseURL = "https://user:pass@cloud.example.test" },
		"authority with colon": func(s *State) { s.AuthorityID = "cloud:1" },
		"empty authority":      func(s *State) { s.AuthorityID = "" },
		"kid not thumbprint":   func(s *State) { s.Key.KID = other.KID },
		"pending equals active": func(s *State) {
			pending := key
			s.PendingKey = &pending
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			state := testState(t, key)
			mutate(&state)
			if err := state.Validate(); err == nil {
				t.Fatal("accepted invalid state")
			}
			store := Store{Path: filepath.Join(t.TempDir(), "installation.json")}
			if err := store.Save(state); err == nil {
				t.Fatal("saved invalid state")
			}
		})
	}
}

// A file written by a newer runtime must refuse rather than be reinterpreted:
// a half-understood installation would sign with the wrong key or re-pair.
func TestHybridStateRefusesNewerVersion(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "installation.json")}
	state := testState(t, testKey(t))
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	generic["version"] = StateVersion + 1
	generic["future_field"] = "set by a newer runtime"
	future, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path, future, stateFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrStateVersion) {
		t.Fatalf("newer state loaded: %v", err)
	}
}

// Rotation replaces the key and its ID together. A reader must never observe
// one without the other, so the write is a rename over a complete file.
func TestHybridStateSaveLeavesNoPartialFile(t *testing.T) {
	directory := t.TempDir()
	store := Store{Path: filepath.Join(directory, "installation.json")}
	first := testState(t, testKey(t))
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Key = testKey(t)
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Key.KID != second.Key.KID {
		t.Fatal("rotation did not replace the active key")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(store.Path) {
			t.Fatalf("save left %s behind", entry.Name())
		}
	}
}

// Anything that renders state for a human or a log must not carry the key.
func TestHybridStateRedactsEveryPrivateKey(t *testing.T) {
	state := testState(t, testKey(t))
	pending := testKey(t)
	state.PendingKey = &pending
	redacted := state.Redacted()
	raw, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("redacted state still carries private key material")
	}
	if redacted.Key.KID != state.Key.KID || redacted.PendingKey.KID != pending.KID {
		t.Fatal("redaction dropped the key identifiers operators need")
	}
	// Redaction must not mutate the caller's state, or the next Save writes a
	// file with no key in it.
	if state.Key.PrivateKeyPEM == "" || state.PendingKey.PrivateKeyPEM == "" {
		t.Fatal("redaction mutated the original state")
	}
}

// The Cloud M2M inventory admits exactly these members with these values.
func TestHybridPublicJWKMatchesAdmissionShape(t *testing.T) {
	key := testKey(t)
	raw, err := key.PublicJWK()
	if err != nil {
		t.Fatal(err)
	}
	var jwk map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&jwk); err != nil {
		t.Fatal(err)
	}
	if len(jwk) != 7 {
		t.Fatalf("public JWK has %d members, want exactly 7: %v", len(jwk), jwk)
	}
	for member, want := range map[string]any{"kty": "RSA", "use": "sig", "alg": "PS256", "kid": key.KID} {
		if jwk[member] != want {
			t.Fatalf("public JWK %s = %v, want %v", member, jwk[member], want)
		}
	}
	operations, ok := jwk["key_ops"].([]any)
	if !ok || len(operations) != 1 || operations[0] != "verify" {
		t.Fatalf("public JWK key_ops = %v, want [verify]", jwk["key_ops"])
	}
	if jwk["n"] == "" || jwk["e"] == "" {
		t.Fatal("public JWK is missing the modulus or exponent")
	}
	if strings.Contains(string(raw), "PRIVATE") || jwk["d"] != nil {
		t.Fatal("public JWK carries private material")
	}
}
