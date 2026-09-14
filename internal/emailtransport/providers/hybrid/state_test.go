package hybrid

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// realPath resolves a path the way confirmChain does, so an expectation is
// written in the spelling the walk actually confirms. A temporary directory
// can sit under a symlink — /var is one on macOS — and comparing against the
// unresolved spelling would test the test, not the walk.
func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

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

// The one Save outcome a normal filesystem will not produce: the rename
// succeeded, so the new state is what a reader sees, but its directory entry
// could not be made durable. A caller that has already committed something
// elsewhere has to tell that apart from a failure before the rename.
func TestHybridStateSaveReportsHowFarItGot(t *testing.T) {
	restore := syncDirectoryFunc
	delay := directorySyncRetryDelay
	t.Cleanup(func() { syncDirectoryFunc = restore; directorySyncRetryDelay = delay })
	directorySyncRetryDelay = time.Millisecond

	store := Store{Path: filepath.Join(t.TempDir(), "installation.json")}
	first := testState(t, testKey(t))
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}

	// A persistent post-rename sync failure is retried, then reported as
	// installed-but-unconfirmed.
	attempts := 0
	syncDirectoryFunc = func(string) error {
		attempts++
		return errors.New("input/output error")
	}
	second := first
	second.Key = testKey(t)
	err := store.Save(second)
	if err == nil {
		t.Fatal("a failed directory sync was reported as success")
	}
	if !Unconfirmed(err) {
		t.Fatalf("a post-rename failure was reported as nothing written: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("directory sync was attempted %d time(s); a transient fsync error is worth retrying", attempts)
	}
	// The rename did happen, so the new state is what a reader sees.
	loaded, loadErr := store.Load()
	if loadErr != nil || loaded.Key.KID != second.Key.KID {
		t.Fatalf("the installed state is not readable: %v", loadErr)
	}

	// A transient failure that clears on retry is not reported at all.
	attempts = 0
	syncDirectoryFunc = func(path string) error {
		attempts++
		if attempts == 1 {
			return errors.New("temporary failure")
		}
		return restore(path)
	}
	third := second
	third.Key = testKey(t)
	if err := store.Save(third); err != nil {
		t.Fatalf("a transient sync failure was not absorbed by the retry: %v", err)
	}

	// And a failure before the rename says nothing was written. A state that
	// does not validate never reaches the filesystem at all.
	syncDirectoryFunc = restore
	invalid := third
	invalid.OrgID = "not-a-uuid"
	if err := store.Save(invalid); err == nil {
		t.Fatal("saved invalid state")
	} else if Unconfirmed(err) {
		t.Fatalf("a pre-rename failure claimed the state was installed: %v", err)
	}
	final, err := store.Load()
	if err != nil || final.Key.KID != third.Key.KID {
		t.Fatalf("a refused save disturbed the installed state: %v", err)
	}
}

// A filesystem without directory synchronization returns fs.ErrInvalid.
// Treating that as success would turn a durability operation that never
// happened into a claim that it did — for a saved key and for a removed one
// alike, where an operator would believe a revoked key is gone while a power
// loss can still bring it back.
func TestHybridStateRefusesToClaimUnsupportedSyncIsDurable(t *testing.T) {
	restore := syncDirectoryFunc
	delay := directorySyncRetryDelay
	t.Cleanup(func() { syncDirectoryFunc = restore; directorySyncRetryDelay = delay })
	directorySyncRetryDelay = time.Millisecond

	for name, injected := range map[string]error{
		"unsupported on this filesystem": fs.ErrInvalid,
		"device error":                   errors.New("input/output error"),
	} {
		t.Run(name, func(t *testing.T) {
			syncDirectoryFunc = restore
			store := Store{Path: filepath.Join(t.TempDir(), "installation.json")}
			state := testState(t, testKey(t))
			if err := store.Save(state); err != nil {
				t.Fatal(err)
			}
			syncDirectoryFunc = func(string) error { return injected }

			next := state
			next.Key = testKey(t)
			saveErr := store.Save(next)
			if saveErr == nil {
				t.Fatal("an unconfirmed write was reported as durable")
			}
			if !Unconfirmed(saveErr) || !errors.Is(saveErr, injected) {
				t.Fatalf("save error %v does not report an unconfirmed change", saveErr)
			}

			removeErr := store.Remove()
			if removeErr == nil {
				t.Fatal("an unconfirmed removal was reported as durable")
			}
			if !Unconfirmed(removeErr) || !errors.Is(removeErr, injected) {
				t.Fatalf("remove error %v does not report an unconfirmed change", removeErr)
			}
			// The change itself did happen; only its durability is unproven.
			if _, err := os.Stat(store.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the file survived a removal that reported only unconfirmed durability: %v", err)
			}
		})
	}
}

// A first installation usually lands on a path that does not exist yet, and
// os.MkdirAll creates the whole chain at once. Each of those new directory
// entries lives in its parent, so syncing only the destination would let Save
// report success while a power loss discards the directory holding the key —
// leaving the runtime without the one credential that proves its identity.
func TestHybridStateSaveSynchronizesEveryDirectoryItCreates(t *testing.T) {
	restore := syncDirectoryFunc
	delay := directorySyncRetryDelay
	t.Cleanup(func() { syncDirectoryFunc = restore; directorySyncRetryDelay = delay })
	directorySyncRetryDelay = time.Millisecond

	existing := t.TempDir()
	// Three absent components: the destination and two ancestors.
	destination := filepath.Join(existing, "state", "hybrid", "installation")

	var synced []string
	syncDirectoryFunc = func(path string) error {
		synced = append(synced, path)
		return restore(path)
	}

	store := Store{Path: filepath.Join(destination, "installation.json")}
	if err := store.Save(testState(t, testKey(t))); err != nil {
		t.Fatal(err)
	}

	// Every new entry is recorded by its parent, and the destination itself
	// is recorded by the post-rename confirmation.
	root := realPath(t, existing)
	for _, required := range []string{
		root,
		filepath.Join(root, "state"),
		filepath.Join(root, "state", "hybrid"),
		filepath.Join(root, "state", "hybrid", "installation"),
	} {
		if !slices.Contains(synced, required) {
			t.Errorf("Save did not synchronize %q; synchronized %q", required, synced)
		}
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("the installed state is not readable: %v", err)
	}
	if loaded.OrgID == "" {
		t.Fatal("the installed state is empty")
	}
}

// The obligation is discharged on every save, not only when a directory is
// missing. A save into a directory that already exists still confirms the
// whole chain: that directory may have been created by an earlier save which
// failed to confirm it, and there is nowhere durable to have recorded that.
// Treating existence as proof would skip the synchronization that was owed
// and report success over an ancestor that can still disappear.
func TestHybridStateSaveConfirmsTheChainEvenWhenNothingIsCreated(t *testing.T) {
	restore := syncDirectoryFunc
	delay := directorySyncRetryDelay
	t.Cleanup(func() { syncDirectoryFunc = restore; directorySyncRetryDelay = delay })
	directorySyncRetryDelay = time.Millisecond

	directory := t.TempDir()
	store := Store{Path: filepath.Join(directory, "installation.json")}

	var synced []string
	syncDirectoryFunc = func(path string) error {
		synced = append(synced, path)
		return restore(path)
	}
	if err := store.Save(testState(t, testKey(t))); err != nil {
		t.Fatal(err)
	}
	root := realPath(t, directory)
	for _, required := range []string{root, filepath.Dir(root), string(filepath.Separator)} {
		if !slices.Contains(synced, required) {
			t.Errorf("a save into an existing directory did not confirm %q; confirmed %q", required, synced)
		}
	}
}

// A directory an earlier save created but could not confirm is left on disk,
// and no durable record of that debt can exist. The retry must therefore
// synchronize every parent again rather than read the directory's existence
// as proof that it is durable — otherwise it writes the sole installation key
// and reports success over an ancestor a power loss can still take.
func TestHybridStateSaveRetriesTheSynchronizationAFailedSaveOwed(t *testing.T) {
	restore := syncDirectoryFunc
	delay := directorySyncRetryDelay
	t.Cleanup(func() { syncDirectoryFunc = restore; directorySyncRetryDelay = delay })
	directorySyncRetryDelay = time.Millisecond

	existing := t.TempDir()
	store := Store{Path: filepath.Join(existing, "state", "hybrid", "installation.json")}

	// The first save creates the tree and then fails to confirm it.
	syncDirectoryFunc = func(string) error { return errors.New("input/output error") }
	if err := store.Save(testState(t, testKey(t))); err == nil {
		t.Fatal("an unconfirmed directory chain was reported as success")
	}

	// The directories survive it, so the retry owes every synchronization.
	var synced []string
	syncDirectoryFunc = func(path string) error {
		synced = append(synced, path)
		return restore(path)
	}
	if err := store.Save(testState(t, testKey(t))); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	root := realPath(t, existing)
	for _, required := range []string{
		root,
		filepath.Join(root, "state"),
		filepath.Join(root, "state", "hybrid"),
	} {
		if !slices.Contains(synced, required) {
			t.Errorf("the retry reported success without confirming %q; confirmed %q", required, synced)
		}
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("the installed state is not readable: %v", err)
	}
}

// hybrid.state_path may be given relative to the working directory. A
// relative walk terminates at "." — filepath.Dir(".") is "." — so it would
// confirm a step or two and return, never reaching the working directory's
// own ancestors: the same unconfirmed-ancestor gap the chain exists to close,
// reached by a different spelling of the same file.
func TestHybridStateSaveConfirmsTheAbsoluteChainForARelativePath(t *testing.T) {
	for _, relative := range []string{
		"installation.json",
		filepath.Join("state", "hybrid", "installation.json"),
		filepath.Join("state", "..", "state", "installation.json"),
	} {
		t.Run(relative, func(t *testing.T) {
			restore := syncDirectoryFunc
			delay := directorySyncRetryDelay
			t.Cleanup(func() { syncDirectoryFunc = restore; directorySyncRetryDelay = delay })
			directorySyncRetryDelay = time.Millisecond

			working := t.TempDir()
			t.Chdir(working)

			var synced []string
			syncDirectoryFunc = func(path string) error {
				synced = append(synced, path)
				return restore(path)
			}
			store := Store{Path: relative}
			if err := store.Save(testState(t, testKey(t))); err != nil {
				t.Fatal(err)
			}

			// Nothing relative is ever confirmed, and the walk runs all the
			// way out to the filesystem root.
			for _, entry := range synced {
				if !filepath.IsAbs(entry) {
					t.Errorf("confirmed a relative entry %q; confirmed %q", entry, synced)
				}
			}
			root := string(filepath.Separator)
			if !slices.Contains(synced, root) {
				t.Errorf("the walk stopped before the filesystem root; confirmed %q", synced)
			}
			// The working directory itself is an ancestor that a relative
			// walk would never have reached.
			absoluteWorking := realPath(t, working)
			if !slices.Contains(synced, absoluteWorking) {
				t.Errorf("the working directory %q was not confirmed; confirmed %q", absoluteWorking, synced)
			}
			if _, err := store.Load(); err != nil {
				t.Fatalf("the installed state is not readable: %v", err)
			}
		})
	}
}

// An operator may point hybrid.state_path through a symlink — a moved data
// volume usually looks exactly like this. filepath.Abs keeps the link in the
// spelling, and opening the directory follows it, so the destination is
// confirmed either way; the real ancestors above the link target are not.
// Those hold the entry the installation key ultimately depends on.
func TestHybridStateSaveConfirmsTheRealChainThroughASymlink(t *testing.T) {
	restore := syncDirectoryFunc
	delay := directorySyncRetryDelay
	t.Cleanup(func() { syncDirectoryFunc = restore; directorySyncRetryDelay = delay })
	directorySyncRetryDelay = time.Millisecond

	base := t.TempDir()
	// The real tree the state actually lives in, and a link pointing at it
	// from somewhere else entirely.
	target := filepath.Join(base, "volume", "hybrid")
	if err := os.MkdirAll(target, stateDirMode); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}

	var synced []string
	syncDirectoryFunc = func(path string) error {
		synced = append(synced, path)
		return restore(path)
	}
	store := Store{Path: filepath.Join(link, "installation.json")}
	if err := store.Save(testState(t, testKey(t))); err != nil {
		t.Fatal(err)
	}

	// The parent of the link target is the entry a lexical walk skips.
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		filepath.Join(resolvedBase, "volume"),
		resolvedBase,
	} {
		if !slices.Contains(synced, required) {
			t.Errorf("the real ancestor %q was not confirmed; confirmed %q", required, synced)
		}
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("the installed state is not readable: %v", err)
	}
}
