package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"neuralmail/internal/config"
	hybridtransport "neuralmail/internal/emailtransport/providers/hybrid"
	"neuralmail/internal/store"
)

func writeConnectedState(t *testing.T, path string, mutate func(*hybridtransport.State)) hybridtransport.State {
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

func hybridTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Hybrid.StatePath = filepath.Join(t.TempDir(), "installation.json")
	// The default address is a real port a developer may be serving on, and
	// the running-runtime guard probes it. Point at one nothing can bind.
	cfg.HTTP.Addr = "127.0.0.1:1"
	return cfg
}

// Without a state path there is nowhere to keep the private key, so every
// subcommand must say that rather than write one to a default location the
// operator did not choose.
func TestHybridCommandRequiresAnExplicitStatePath(t *testing.T) {
	cfg := config.Default()
	for _, command := range [][]string{{"connect"}, {"status"}, {"rotate"}, {"disconnect"}} {
		var out bytes.Buffer
		err := runHybrid(context.Background(), cfg, command, &out)
		if err == nil || !strings.Contains(err.Error(), "state_path") {
			t.Fatalf("%v: %v", command, err)
		}
	}
}

func TestHybridCommandRejectsUnknownSubcommands(t *testing.T) {
	cfg := hybridTestConfig(t)
	var out bytes.Buffer
	if err := runHybrid(context.Background(), cfg, nil, &out); err == nil {
		t.Fatal("accepted an empty subcommand")
	}
	if err := runHybrid(context.Background(), cfg, []string{"revoke"}, &out); err == nil {
		t.Fatal("accepted an unknown subcommand")
	}
	// `revoke` in particular must not look like it worked: only the mailbox
	// owner can revoke, and an operator who believes this did would leave a
	// live installation behind.
	if out.Len() != 0 {
		t.Fatalf("unknown subcommand produced output: %q", out.String())
	}
}

// The first connect prints the key an operator has to admit, before anything
// contacts Cloud. Nothing it prints may carry private material.
func TestHybridConnectPrintsAnAdmissionRecordWithoutPrivateMaterial(t *testing.T) {
	cfg := hybridTestConfig(t)
	var out bytes.Buffer
	// Cloud is unreachable here on purpose: the admission record must be
	// produced before the network is needed, or an operator cannot get past
	// the first step.
	err := runHybrid(context.Background(), cfg, []string{
		"connect",
		"-cloud-url", "https://cloud.example.test",
		"-token-endpoint", "https://auth.example.test/oauth/token",
		"-resource", "https://runtime.example.test/mcp",
		"-client-id", "hybrid-client",
		"-generation", "1",
		"-cloud-inbox-id", uuid.NewString(),
		"-authority-id", "cloud.example.test",
	}, &out)
	if err == nil {
		t.Fatal("expected the unreachable cloud to fail the pairing")
	}
	printed := out.String()
	if !strings.Contains(printed, "Admit this public key") {
		t.Fatalf("no admission instructions: %q", printed)
	}
	if strings.Contains(printed, "PRIVATE KEY") || strings.Contains(printed, "private_key_pem") {
		t.Fatal("admission output carries private key material")
	}
	var record hybridtransport.AdmissionRecord
	block := printed[strings.Index(printed, "{"):]
	if err := json.Unmarshal([]byte(block[:strings.LastIndex(block, "}")+1]), &record); err != nil {
		t.Fatalf("admission record is not valid JSON: %v", err)
	}
	if record.KeyID == "" || record.ClientID != "hybrid-client" || len(record.PublicJWK) == 0 {
		t.Fatalf("incomplete admission record: %+v", record)
	}
	// The key is on disk so a resumed connect reuses it.
	state, err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load()
	if err != nil || state.Key.KID != record.KeyID {
		t.Fatalf("key was not persisted for a resumed connect: %v", err)
	}
}

// Resuming does not ask the operator to retype the Cloud parameters, and must
// not mint a second key they would have to admit again.
func TestHybridConnectResumesWithoutRepeatingParameters(t *testing.T) {
	cfg := hybridTestConfig(t)
	first := bytes.Buffer{}
	args := []string{
		"connect",
		"-cloud-url", "https://cloud.example.test",
		"-token-endpoint", "https://auth.example.test/oauth/token",
		"-resource", "https://runtime.example.test/mcp",
		"-client-id", "hybrid-client",
		"-generation", "1",
		"-cloud-inbox-id", uuid.NewString(),
		"-authority-id", "cloud.example.test",
	}
	_ = runHybrid(context.Background(), cfg, args, &first)
	before, err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := runHybrid(context.Background(), cfg, []string{"connect"}, &second); err == nil {
		t.Fatal("expected the unreachable cloud to fail again")
	}
	after, err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.Key.KID != before.Key.KID {
		t.Fatal("resuming replaced a key the operator may already have admitted")
	}
	if after.CloudBaseURL != before.CloudBaseURL || after.ClientID != before.ClientID {
		t.Fatal("resuming lost the recorded cloud parameters")
	}
}

// Disconnect must remove the key and must not claim to have revoked anything.
func TestHybridDisconnectSaysItDidNotRevoke(t *testing.T) {
	cfg := hybridTestConfig(t)
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
	if err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Save(state); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHybrid(context.Background(), cfg, []string{"disconnect"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Hybrid.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private key survived disconnect: %v", err)
	}
	if !strings.Contains(out.String(), "did not revoke") {
		t.Fatalf("disconnect did not say revocation is still required: %q", out.String())
	}
}

// Preparing a rotation prints the replacement to admit and states plainly that
// the runtime has not switched yet, so nobody removes the live key too early.
func TestHybridRotatePreparesWithoutSwitching(t *testing.T) {
	cfg := hybridTestConfig(t)
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
	stateStore := hybridtransport.Store{Path: cfg.Hybrid.StatePath}
	if err := stateStore.Save(state); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHybrid(context.Background(), cfg, []string{"rotate"}, &out); err != nil {
		t.Fatal(err)
	}
	printed := out.String()
	if !strings.Contains(printed, "keeps using its current key") {
		t.Fatalf("rotate did not say the switch has not happened: %q", printed)
	}
	if strings.Contains(printed, "PRIVATE KEY") {
		t.Fatal("rotate printed private key material")
	}
	held, err := stateStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if held.Key.KID != key.KID || held.PendingKey == nil {
		t.Fatal("preparing a rotation changed the active key")
	}

	// -commit and -abandon are mutually exclusive; taking both would be
	// ambiguous about which key survives.
	var conflict bytes.Buffer
	if err := runHybrid(context.Background(), cfg, []string{"rotate", "-commit", "-abandon"}, &conflict); err == nil {
		t.Fatal("accepted both -commit and -abandon")
	}

	var abandon bytes.Buffer
	if err := runHybrid(context.Background(), cfg, []string{"rotate", "-abandon"}, &abandon); err != nil {
		t.Fatal(err)
	}
	final, err := stateStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if final.PendingKey != nil || final.Key.KID != key.KID {
		t.Fatal("abandon did not restore the runtime to its active key alone")
	}
}

// Status is a read: it must not create state, and it must report an
// unconnected runtime plainly rather than failing in a way that looks broken.
func TestHybridStatusOnAnUnconnectedRuntime(t *testing.T) {
	cfg := hybridTestConfig(t)
	var out bytes.Buffer
	err := runHybrid(context.Background(), cfg, []string{"status"}, &out)
	if !errors.Is(err, hybridtransport.ErrNotConnected) {
		t.Fatalf("status on an unconnected runtime: %v", err)
	}
	if _, statErr := os.Stat(cfg.Hybrid.StatePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("status created installation state")
	}
}

// connect, rotate -commit and disconnect all replace something a serving
// daemon already loaded. Reporting success while the runtime goes on using
// the previous installation is how an operator is told to remove a Cloud key
// the runtime is still signing with.
func TestHybridMutationsRefuseWhileTheRuntimeIsServing(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer live.Close()

	for name, args := range map[string][]string{
		"connect":    {"connect"},
		"rotate":     {"rotate", "-commit"},
		"disconnect": {"disconnect"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := hybridTestConfig(t)
			cfg.HTTP.Addr = strings.TrimPrefix(live.URL, "http://")
			writeConnectedState(t, cfg.Hybrid.StatePath, nil)
			var out bytes.Buffer
			err := runHybrid(context.Background(), cfg, args, &out)
			if err == nil || !strings.Contains(err.Error(), "still serving") {
				t.Fatalf("%s did not refuse against a serving runtime: %v", name, err)
			}
			// Nothing may have changed on disk.
			if _, statErr := os.Stat(cfg.Hybrid.StatePath); statErr != nil {
				t.Fatalf("%s removed the installation anyway: %v", name, statErr)
			}
		})
	}

	// An operator who will restart immediately can override, and a runtime
	// that is not serving is not blocked at all.
	cfg := hybridTestConfig(t)
	cfg.HTTP.Addr = strings.TrimPrefix(live.URL, "http://")
	writeConnectedState(t, cfg.Hybrid.StatePath, nil)
	var out bytes.Buffer
	err := runHybrid(context.Background(), cfg, []string{"rotate", "-commit", "-allow-running"}, &out)
	if err == nil || strings.Contains(err.Error(), "still serving") {
		t.Fatalf("-allow-running did not bypass the guard: %v", err)
	}

	stopped := hybridTestConfig(t)
	stopped.HTTP.Addr = "127.0.0.1:1"
	writeConnectedState(t, stopped.Hybrid.StatePath, nil)
	var quiet bytes.Buffer
	err = runHybrid(context.Background(), stopped, []string{"rotate", "-commit"}, &quiet)
	if err == nil || strings.Contains(err.Error(), "still serving") {
		t.Fatalf("a stopped runtime was treated as serving: %v", err)
	}
}

// Disconnect must put the mailbox back. Leaving it on "hybrid" after the
// adapters are gone stops the poll loop with "unknown inbound provider" and
// requeues every outbound row forever, so disconnecting would take the whole
// runtime down with it.
func TestHybridDisconnectRestoresMailboxRoutingBeforeRemovingTheKey(t *testing.T) {
	cfg := hybridTestConfig(t)
	cfg.HTTP.Addr = "127.0.0.1:1"
	// A DSN that cannot be reached: restoring the routing must fail, and the
	// key must then still be on disk, because a runtime with a stranded
	// mailbox is worse than one with a stale key.
	cfg.Database.DSN = "postgres://127.0.0.1:1/absent?sslmode=disable"
	writeConnectedState(t, cfg.Hybrid.StatePath, func(s *hybridtransport.State) {
		s.LocalMailbox = &hybridtransport.LocalMailbox{
			InboxID: uuid.NewString(), Address: "agent@example.test",
			PriorInbound: "jmap", PriorOutbound: "smtp",
		}
	})
	var out bytes.Buffer
	err := runHybrid(context.Background(), cfg, []string{"disconnect"}, &out)
	if err == nil || !strings.Contains(err.Error(), "was not restored") {
		t.Fatalf("disconnect did not stop on a failed restore: %v", err)
	}
	if _, statErr := os.Stat(cfg.Hybrid.StatePath); statErr != nil {
		t.Fatalf("the key was removed even though the mailbox was left stranded: %v", statErr)
	}
}

// An installation with no recorded mailbox still disconnects: a state file
// written before the binding existed must not become impossible to remove.
func TestHybridDisconnectWithoutARecordedMailbox(t *testing.T) {
	cfg := hybridTestConfig(t)
	cfg.HTTP.Addr = "127.0.0.1:1"
	writeConnectedState(t, cfg.Hybrid.StatePath, nil)
	var out bytes.Buffer
	if err := runHybrid(context.Background(), cfg, []string{"disconnect"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Hybrid.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private key survived disconnect: %v", err)
	}
	if !strings.Contains(out.String(), "Restart the runtime") {
		t.Fatalf("disconnect did not tell the operator to restart: %q", out.String())
	}
}

// hybridRuntimeDatabase gives the CLI a real store. Mailbox routing is the
// thing under test here and it lives in PostgreSQL, so a fake would prove
// nothing about what connect and disconnect actually leave behind.
func hybridRuntimeDatabase(t *testing.T) (config.Config, *store.Store, string) {
	t.Helper()
	dsn := os.Getenv("NM_TEST_DB_DSN")
	if dsn == "" {
		if os.Getenv("NM_REQUIRE_DB") == "1" {
			t.Fatal("NM_REQUIRE_DB=1 but NM_TEST_DB_DSN is unset")
		}
		t.Skip("set NM_TEST_DB_DSN to exercise hybrid mailbox routing")
	}
	cfg := hybridTestConfig(t)
	cfg.Database.DSN = dsn
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := store.MigrateCore(ctx, st.DB()); err != nil {
		t.Fatal(err)
	}
	address := fmt.Sprintf("hybrid-cli-%s@local.nerve.email", strings.ReplaceAll(uuid.NewString(), "-", ""))
	cfg.SMTP.From = address
	return cfg, st, address
}

func inboxProviders(t *testing.T, st *store.Store, address string) (string, string, string) {
	t.Helper()
	record, err := st.GetInboxByAddress(context.Background(), address)
	if err != nil {
		t.Fatal(err)
	}
	return record.ID, record.InboundProvider, record.OutboundProvider
}

// Re-running connect against an installation that is already bound must not
// rebind. Capturing the routing a second time would snapshot "hybrid" as the
// providers to restore and lose the real ones, and binding a second mailbox
// would leave the first on a provider nothing polls or later restores.
func TestHybridConnectRebindIsIdempotentAndRefusesADifferentMailbox(t *testing.T) {
	cfg, st, address := hybridRuntimeDatabase(t)
	ctx := context.Background()
	writeConnectedState(t, cfg.Hybrid.StatePath, nil)

	var first bytes.Buffer
	if err := runHybrid(ctx, cfg, []string{"connect"}, &first); err != nil {
		t.Fatal(err)
	}
	inboxID, inbound, outbound := inboxProviders(t, st, address)
	if inbound != "hybrid" || outbound != "hybrid" {
		t.Fatalf("mailbox routed to %s/%s, want hybrid both ways", inbound, outbound)
	}
	bound, err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if bound.LocalMailbox == nil || bound.LocalMailbox.InboxID != inboxID {
		t.Fatalf("binding not recorded: %+v", bound.LocalMailbox)
	}
	// The providers to restore are the real ones, not "hybrid".
	if bound.LocalMailbox.PriorInbound == "hybrid" || bound.LocalMailbox.PriorOutbound == "hybrid" {
		t.Fatalf("recorded hybrid as the routing to restore: %+v", bound.LocalMailbox)
	}
	prior := *bound.LocalMailbox

	// Same mailbox again: a no-op that keeps the original snapshot.
	var second bytes.Buffer
	if err := runHybrid(ctx, cfg, []string{"connect"}, &second); err != nil {
		t.Fatal(err)
	}
	rebound, err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if *rebound.LocalMailbox != prior {
		t.Fatalf("rerunning connect changed the binding: %+v -> %+v", prior, *rebound.LocalMailbox)
	}
	if !strings.Contains(second.String(), "already sends and receives") {
		t.Fatalf("rerun did not report the existing binding: %q", second.String())
	}

	// A different mailbox is refused rather than silently stranding the first.
	other := fmt.Sprintf("hybrid-other-%s@local.nerve.email", strings.ReplaceAll(uuid.NewString(), "-", ""))
	var third bytes.Buffer
	err = runHybrid(ctx, cfg, []string{"connect", "-local-inbox", other}, &third)
	if err == nil || !strings.Contains(err.Error(), "disconnect before binding") {
		t.Fatalf("connect rebound a different mailbox: %v", err)
	}
	var routed int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM inboxes WHERE inbound_provider = 'hybrid' AND address IN ($1, $2)`,
		address, other).Scan(&routed); err != nil {
		t.Fatal(err)
	}
	if routed != 1 {
		t.Fatalf("%d mailboxes route through hybrid, want exactly 1", routed)
	}

	// Disconnect puts the original providers back.
	var fourth bytes.Buffer
	if err := runHybrid(ctx, cfg, []string{"disconnect"}, &fourth); err != nil {
		t.Fatal(err)
	}
	_, restoredIn, restoredOut := inboxProviders(t, st, address)
	if restoredIn == "hybrid" || restoredOut == "hybrid" {
		t.Fatalf("disconnect left the mailbox on hybrid: %s/%s", restoredIn, restoredOut)
	}
}

// The routing update commits before the state file is written. If that write
// fails the mailbox is on hybrid with nothing recording it: the runtime would
// poll the configured default and disconnect would have no routing to
// restore, stranding the mailbox in both directions.
func TestHybridConnectUndoesRoutingWhenTheBindingCannotBeRecorded(t *testing.T) {
	cfg, st, address := hybridRuntimeDatabase(t)
	ctx := context.Background()
	writeConnectedState(t, cfg.Hybrid.StatePath, nil)
	// Give the mailbox a routing worth returning to, rather than letting
	// connect create it: the point is that a real prior setup survives.
	inboxID, err := st.EnsureDefaults(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInboxProviders(ctx, inboxID, "jmap", "resend"); err != nil {
		t.Fatal(err)
	}
	_, beforeIn, beforeOut := inboxProviders(t, st, address)

	// A state directory that cannot take a new file: Save writes through a
	// temporary file in it, while the existing state still reads.
	directory := filepath.Dir(cfg.Hybrid.StatePath)
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })

	var out bytes.Buffer
	err = runHybrid(ctx, cfg, []string{"connect"}, &out)
	if err == nil {
		t.Fatal("connect reported success though the binding was never recorded")
	}
	if !strings.Contains(err.Error(), "returned to its previous providers") {
		t.Fatalf("connect did not undo the routing: %v", err)
	}
	_, afterIn, afterOut := inboxProviders(t, st, address)
	if afterIn != beforeIn || afterOut != beforeOut {
		t.Fatalf("mailbox left on %s/%s, want the pre-connect %s/%s", afterIn, afterOut, beforeIn, beforeOut)
	}
	state, err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.LocalMailbox != nil {
		t.Fatalf("durable state kept a binding that was never committed: %+v", state.LocalMailbox)
	}
}

// This repository has one canonical-equivalence rule for inbox addresses, and
// the store applies it. Comparing address text instead would reject an
// equivalent spelling of the mailbox already bound and force an operator to
// disconnect for no reason.
func TestHybridConnectComparesMailboxesCanonically(t *testing.T) {
	cfg, st, address := hybridRuntimeDatabase(t)
	ctx := context.Background()
	writeConnectedState(t, cfg.Hybrid.StatePath, nil)
	var first bytes.Buffer
	if err := runHybrid(ctx, cfg, []string{"connect"}, &first); err != nil {
		t.Fatal(err)
	}
	bound, err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	prior := *bound.LocalMailbox

	local, domain, ok := strings.Cut(address, "@")
	if !ok {
		t.Fatalf("unexpected fixture address %q", address)
	}
	equivalents := map[string]string{
		"uppercase domain":  local + "@" + strings.ToUpper(domain),
		"mixed case local":  strings.ToUpper(local[:1]) + local[1:] + "@" + domain,
		"trailing dot":      address + ".",
		"surrounding space": " " + address + " ",
	}
	for name, spelling := range equivalents {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := runHybrid(ctx, cfg, []string{"connect", "-local-inbox", spelling}, &out); err != nil {
				t.Fatalf("%q was treated as a different mailbox: %v", spelling, err)
			}
			if !strings.Contains(out.String(), "already sends and receives") {
				t.Fatalf("%q did not resolve to the bound mailbox: %q", spelling, out.String())
			}
			again, err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load()
			if err != nil {
				t.Fatal(err)
			}
			if *again.LocalMailbox != prior {
				t.Fatalf("%q changed the binding: %+v", spelling, *again.LocalMailbox)
			}
		})
	}

	// A genuinely different mailbox is still refused, and looking it up must
	// not create one on the way to refusing.
	other := fmt.Sprintf("hybrid-distinct-%s@local.nerve.email", strings.ReplaceAll(uuid.NewString(), "-", ""))
	var refused bytes.Buffer
	if err := runHybrid(ctx, cfg, []string{"connect", "-local-inbox", other}, &refused); err == nil {
		t.Fatal("a different mailbox was accepted")
	}
	var created int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE address = $1`, other).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatal("refusing a different mailbox created it anyway")
	}
}

// Save reports which side of the rename it failed on, and the two need
// opposite handling. A pre-rename failure changed nothing, so the routing
// must be undone. A post-rename failure leaves the new state readable but
// not proven durable, so undoing the routing would contradict the file — and
// neither outcome may be reported as success, because a binding that vanishes
// on the next reboot strands the mailbox.
func TestHybridConnectHandlesBothSidesOfAnUncertainSave(t *testing.T) {
	ctx := context.Background()

	t.Run("installed but unconfirmed", func(t *testing.T) {
		cfg, st, address := hybridRuntimeDatabase(t)
		state := writeConnectedState(t, cfg.Hybrid.StatePath, nil)
		inboxID, err := st.EnsureDefaults(ctx, address)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateInboxProviders(ctx, inboxID, "jmap", "resend"); err != nil {
			t.Fatal(err)
		}
		mailbox := hybridtransport.LocalMailbox{
			InboxID: inboxID, Address: address, PriorInbound: "jmap", PriorOutbound: "resend",
		}
		state.LocalMailbox = &mailbox
		if err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Save(state); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateInboxTransportProviders(ctx, inboxID, "hybrid"); err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer
		err = reconcileBinding(ctx, cfg, hybridtransport.Store{Path: cfg.Hybrid.StatePath}, mailbox,
			&hybridtransport.UnconfirmedError{Op: "write", Err: errors.New("input/output error")}, &out)
		if err == nil || !strings.Contains(err.Error(), "not proven durable") {
			t.Fatalf("an unproven binding was reported as finished: %v", err)
		}
		// The file describes this routing, so it stays.
		_, inbound, outbound := inboxProviders(t, st, address)
		if inbound != "hybrid" || outbound != "hybrid" {
			t.Fatalf("routing was rolled back under an installed binding: %s/%s", inbound, outbound)
		}
		if !strings.Contains(out.String(), "survive a reboot") {
			t.Fatalf("the operator was not told what to check: %q", out.String())
		}
	})

	t.Run("nothing written", func(t *testing.T) {
		cfg, st, address := hybridRuntimeDatabase(t)
		writeConnectedState(t, cfg.Hybrid.StatePath, nil)
		inboxID, err := st.EnsureDefaults(ctx, address)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateInboxTransportProviders(ctx, inboxID, "hybrid"); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = reconcileBinding(ctx, cfg, hybridtransport.Store{Path: cfg.Hybrid.StatePath},
			hybridtransport.LocalMailbox{InboxID: inboxID, Address: address,
				PriorInbound: "jmap", PriorOutbound: "smtp"},
			errors.New("no space left on device"), &out)
		if err == nil || !strings.Contains(err.Error(), "returned to its previous providers") {
			t.Fatalf("an unrecorded binding was not undone: %v", err)
		}
		_, inbound, outbound := inboxProviders(t, st, address)
		if inbound == "hybrid" || outbound == "hybrid" {
			t.Fatalf("mailbox left on hybrid with nothing recording it: %s/%s", inbound, outbound)
		}
		if state, loadErr := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load(); loadErr != nil || state.LocalMailbox != nil {
			t.Fatalf("durable state kept a binding that was never committed: %v", loadErr)
		}
	})
}

// A run whose directory sync never succeeded tells the operator to re-run
// connect to confirm the binding. A rerun that reported success without
// performing any durability operation would confirm nothing, so this checks
// that the rerun actually rewrites the state file — which is what retries the
// sync — rather than returning from a fast path.
func TestHybridConnectRerunPerformsTheDurabilityOperationItPromises(t *testing.T) {
	cfg, st, address := hybridRuntimeDatabase(t)
	ctx := context.Background()
	writeConnectedState(t, cfg.Hybrid.StatePath, nil)

	var first bytes.Buffer
	if err := runHybrid(ctx, cfg, []string{"connect"}, &first); err != nil {
		t.Fatal(err)
	}
	bound, err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(cfg.Hybrid.StatePath)
	if err != nil {
		t.Fatal(err)
	}

	var rerun bytes.Buffer
	if err := runHybrid(ctx, cfg, []string{"connect"}, &rerun); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(cfg.Hybrid.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	// Save installs by rename, so a rerun that actually re-wrote the state
	// leaves a different file behind. Same file means the fast path returned
	// success without touching the disk.
	if os.SameFile(before, after) {
		t.Fatal("the rerun reported success without rewriting the installation file")
	}
	// And it changed nothing else: same binding, same routing, same key.
	again, err := (hybridtransport.Store{Path: cfg.Hybrid.StatePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if *again.LocalMailbox != *bound.LocalMailbox || again.Key.KID != bound.Key.KID {
		t.Fatalf("the rerun altered the installation: %+v", again.Redacted())
	}
	_, inbound, outbound := inboxProviders(t, st, address)
	if inbound != "hybrid" || outbound != "hybrid" {
		t.Fatalf("the rerun disturbed the routing: %s/%s", inbound, outbound)
	}

	// When the confirmation still cannot be made, the rerun must not claim
	// the binding is good, and must leave the routing alone.
	directory := filepath.Dir(cfg.Hybrid.StatePath)
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })
	var blocked bytes.Buffer
	err = runHybrid(ctx, cfg, []string{"connect"}, &blocked)
	if err == nil || !strings.Contains(err.Error(), "durability is unconfirmed") {
		t.Fatalf("a rerun that could not confirm reported success: %v", err)
	}
	_, stillIn, stillOut := inboxProviders(t, st, address)
	if stillIn != "hybrid" || stillOut != "hybrid" {
		t.Fatalf("a failed confirmation rolled back a binding that is recorded: %s/%s", stillIn, stillOut)
	}
}
