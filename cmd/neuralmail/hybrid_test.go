package main

import (
	"bytes"
	"context"
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

func hybridTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Hybrid.StatePath = filepath.Join(t.TempDir(), "installation.json")
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
