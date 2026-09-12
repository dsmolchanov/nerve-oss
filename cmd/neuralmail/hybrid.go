package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"neuralmail/internal/config"
	hybridtransport "neuralmail/internal/emailtransport/providers/hybrid"
	"neuralmail/internal/store"
)

// hybridConnectTimeout bounds the wait for two people: an operator admitting
// the key and the owner approving the pairing. Cloud expires a pairing after
// ten minutes, so waiting longer than that only hides an expired one.
const hybridConnectTimeout = 10 * time.Minute

func runHybrid(ctx context.Context, cfg config.Config, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: neuralmail hybrid <connect|status|rotate|disconnect>")
	}
	path := strings.TrimSpace(cfg.Hybrid.StatePath)
	if path == "" {
		return errors.New("set hybrid.state_path (or NERVE_HYBRID_STATE_PATH) to the file holding this runtime's installation")
	}
	stateStore := hybridtransport.Store{Path: path}
	client := &http.Client{Timeout: cfg.Hybrid.Timeout}
	switch args[0] {
	case "connect":
		return hybridConnect(ctx, cfg, stateStore, client, args[1:], out)
	case "status":
		return hybridStatus(ctx, stateStore, client, out)
	case "rotate":
		return hybridRotate(ctx, cfg, stateStore, client, args[1:], out)
	case "disconnect":
		return hybridDisconnect(ctx, cfg, stateStore, args[1:], out)
	default:
		return fmt.Errorf("unknown hybrid command %q", args[0])
	}
}

// requireStoppedRuntime refuses a change the serving daemon has already acted
// on.
//
// connect, rotate -commit and disconnect all replace something the runtime
// loaded at startup: the installation key, the registered adapters, or the
// mailbox routing. A daemon that is already serving keeps the old key and the
// old adapters in memory, so the command would report success while the
// runtime went on signing with a key the operator was just told to remove, or
// went on carrying mail for an installation it was told was gone.
//
// The check is a liveness probe against this runtime's own configured
// address. -allow-running is for an operator who will restart immediately and
// accepts the window in between.
func requireStoppedRuntime(ctx context.Context, cfg config.Config, allowRunning bool, operation string) error {
	if allowRunning {
		return nil
	}
	addr := strings.TrimSpace(cfg.HTTP.Addr)
	if addr == "" {
		return nil
	}
	if host, port, err := net.SplitHostPort(addr); err == nil {
		// A daemon bound to every interface answers on loopback, which is the
		// only address this command can reach from inside the same host.
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		addr = net.JoinHostPort(host, port)
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probe, http.MethodGet, "http://"+addr+"/readyz", nil)
	if err != nil {
		return nil
	}
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return nil
	}
	return fmt.Errorf("the runtime at %s is still serving; %s would leave it using the previous "+
		"installation until it restarts. Stop it first, or pass -allow-running and restart immediately", addr, operation)
}

func hybridConnect(ctx context.Context, cfg config.Config, stateStore hybridtransport.Store, client *http.Client, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("hybrid connect", flag.ContinueOnError)
	flags.SetOutput(out)
	cloudURL := flags.String("cloud-url", "", "Cloud base URL serving the hybrid machine API")
	tokenEndpoint := flags.String("token-endpoint", "", "OAuth token endpoint that mints access tokens for this runtime")
	resource := flags.String("resource", "", "OAuth resource indicator the access token is requested for")
	clientID := flags.String("client-id", "", "machine client ID admitted for this runtime")
	generation := flags.Int64("generation", 0, "onboarding generation of that machine client")
	inboxID := flags.String("cloud-inbox-id", "", "UUID of the Cloud mailbox to carry")
	authorityID := flags.String("authority-id", "", "stable name for this Cloud deployment, used to namespace local deduplication")
	localInbox := flags.String("local-inbox", "", "address of the local mailbox to route through Cloud (default: smtp.from)")
	allowRunning := flags.Bool("allow-running", false, "proceed even though the runtime is serving; restart it immediately afterwards")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected hybrid connect arguments")
	}

	// Begin only needs the parameters on a first run. A resumed connect reads
	// them back from the state file, so an operator re-running the command
	// after approving does not have to retype them.
	pairing, loadErr := stateStore.Load()
	if errors.Is(loadErr, hybridtransport.ErrNotConnected) {
		params := hybridtransport.ConnectParams{
			CloudBaseURL: *cloudURL, TokenEndpoint: *tokenEndpoint, Resource: *resource,
			ClientID: *clientID, Generation: *generation, InboxID: *inboxID, AuthorityID: *authorityID,
		}
		var beginErr error
		pairing, beginErr = hybridtransport.Begin(stateStore, params)
		// A write that landed but could not be confirmed still put the key on
		// disk, and Begin hands it back. Carry on to print it: a run that
		// stopped here would leave the operator with a key they never saw,
		// and the next run would reach Cloud with one never admitted.
		if beginErr != nil && pairing.Key.KID == "" {
			return beginErr
		}
		loadErr = beginErr
	} else if loadErr != nil {
		return loadErr
	}

	if !pairing.Installed() {
		// Every run that resumes a pairing rewrites the state, which is what
		// retries a directory sync that never succeeded, and re-prints the key
		// to admit. Returning early on either would tell the operator to
		// re-run a command that then confirms nothing and shows nothing.
		saveErr := stateStore.Save(pairing)
		if saveErr != nil {
			// Show a key only when the runtime holds it. A rewrite that never
			// landed leaves whatever was already stored, which is still the
			// key to admit; one that could not be written at all on a first
			// run leaves nothing to show.
			if stored, loadErr := stateStore.Load(); loadErr != nil || stored.Key.KID != pairing.Key.KID {
				return fmt.Errorf("the installation key could not be written: %w", saveErr)
			}
		}
		record, err := hybridtransport.Admission(pairing)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Installation key %s.\n", record.KeyID)
		fmt.Fprintln(out, "Admit this public key to the Cloud machine-client inventory before continuing:")
		if err := writeJSONBlock(out, record); err != nil {
			return err
		}
		if saveErr != nil {
			fmt.Fprintln(out, "The key is on disk but the filesystem would not confirm it survives a reboot.")
			fmt.Fprintln(out, "Check the disk, then re-run `hybrid connect`: it rewrites the file and retries the sync.")
			return fmt.Errorf("installation key %s is not confirmed durable: %w", record.KeyID, saveErr)
		}
		if loadErr != nil {
			return loadErr
		}
	}

	if err := requireStoppedRuntime(ctx, cfg, *allowRunning, "connecting"); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, hybridConnectTimeout)
	defer cancel()
	state, err := hybridtransport.Connect(waitCtx, stateStore, client, out)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Connected installation %s in organization %s.\n", state.InstallationID, state.OrgID)

	address := strings.TrimSpace(*localInbox)
	if address == "" {
		address = cfg.SMTP.From
	}
	// A rerun against an installation that is already bound must not rebind.
	// Capturing the routing a second time would snapshot "hybrid" as the
	// providers to restore and lose the real ones, and pointing at a
	// different mailbox would leave the first one on a provider nothing
	// polls or restores.
	if existing := state.LocalMailbox; existing != nil {
		same, err := sameLocalMailbox(ctx, cfg, *existing, address)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("installation %s already carries local mailbox %s; disconnect before binding %s",
				state.InstallationID, existing.Address, address)
		}
		// Re-save rather than returning straight away. A previous run whose
		// directory sync never succeeded tells the operator to re-run this
		// command to confirm the binding, and a path that reports success
		// without performing any durability operation would confirm nothing.
		// Save rewrites the same state and retries the sync.
		if err := stateStore.Save(state); err != nil {
			// The binding is already recorded and the routing already set, so
			// nothing is rolled back here; only the confirmation failed.
			fmt.Fprintln(out, "The installation file could not be confirmed durable. Check the disk before relying on this binding.")
			return fmt.Errorf("local mailbox %s is bound but its durability is unconfirmed: %w", existing.Address, err)
		}
		fmt.Fprintf(out, "Local mailbox %s (%s) already sends and receives through Cloud.\n", existing.Address, existing.InboxID)
		fmt.Fprintln(out, "Restart the runtime so it loads this installation.")
		return nil
	}

	mailbox, err := bindLocalInbox(ctx, cfg, address)
	if err != nil {
		// The installation is real and recorded; only the local routing is
		// missing. Say so precisely rather than implying the pairing failed.
		return fmt.Errorf("installation %s is connected but local mailbox %s was not routed to it: %w",
			state.InstallationID, address, err)
	}
	// Record which mailbox this is and what it was routed to. The runtime
	// polls that mailbox, and disconnect puts the routing back.
	state.LocalMailbox = mailbox
	if saveErr := stateStore.Save(state); saveErr != nil {
		return reconcileBinding(ctx, cfg, stateStore, *mailbox, saveErr, out)
	}
	fmt.Fprintf(out, "Local mailbox %s (%s) now sends and receives through Cloud.\n", address, mailbox.InboxID)
	fmt.Fprintln(out, "Restart the runtime so it loads this installation.")
	return nil
}

// reconcileBinding decides what to do when recording the binding failed.
//
// A save error does not mean nothing was written. The state file is installed
// by rename and its directory entry synced afterwards, so a failure can come
// before the rename — nothing changed — or after it, where the new state is
// what a reader sees but may not survive a power loss. Save reports which,
// and the two need opposite handling: rolling the database back on the second
// would leave the file naming a mailbox whose providers no longer route to
// Cloud.
//
// Neither outcome is reported as success. A binding whose durability is
// unconfirmed can still vanish on the next reboot and strand the mailbox, so
// the operator is told exactly what to check rather than left believing the
// pairing is finished.
func reconcileBinding(ctx context.Context, cfg config.Config, stateStore hybridtransport.Store,
	mailbox hybridtransport.LocalMailbox, saveErr error, out io.Writer) error {
	if hybridtransport.Unconfirmed(saveErr) {
		fmt.Fprintf(out, "Local mailbox %s (%s) is routed through Cloud and the installation file was written,\n",
			mailbox.Address, mailbox.InboxID)
		fmt.Fprintln(out, "but the filesystem would not confirm that the file will survive a reboot.")
		fmt.Fprintln(out, "Check the disk, then re-run `hybrid connect`: it re-writes the file and retries the sync.")
		return fmt.Errorf("installation binding for %s is not proven durable: %w", mailbox.Address, saveErr)
	}
	// Nothing was written, so the routing must not stand on its own.
	if restoreErr := restoreLocalInbox(ctx, cfg, mailbox); restoreErr != nil {
		return fmt.Errorf("local mailbox %s is routed through Cloud, the binding was not recorded (%v), "+
			"and the routing could not be undone: %w", mailbox.Address, saveErr, restoreErr)
	}
	return fmt.Errorf("local mailbox %s was returned to its previous providers because the binding "+
		"could not be recorded: %w", mailbox.Address, saveErr)
}

// sameLocalMailbox reports whether a requested address names the mailbox the
// installation already carries.
//
// It compares resolved inbox identity, not address text. This repository has
// one canonical-equivalence rule for inbox addresses, applied by the store, so
// `Agent@EXAMPLE.COM.` and `agent@example.com` are the same mailbox. Comparing
// the strings would reject an equivalent spelling and force an operator to
// disconnect for no reason.
func sameLocalMailbox(ctx context.Context, cfg config.Config, existing hybridtransport.LocalMailbox, address string) (bool, error) {
	if strings.TrimSpace(address) == "" {
		return false, errors.New("no local mailbox address; pass -local-inbox or set smtp.from")
	}
	st, err := store.Open(cfg.Database.DSN)
	if err != nil {
		return false, err
	}
	defer st.Close()
	// A lookup, never a create: resolving a genuinely different address must
	// not leave a stray mailbox behind on the way to refusing.
	record, err := st.GetInboxByAddress(ctx, address)
	if err != nil {
		// No such mailbox means it is not the one already bound.
		return false, nil
	}
	return record.ID == existing.InboxID, nil
}

// bindLocalInbox points one local mailbox at the hybrid transport and returns
// what it was pointed at before.
//
// It is done here, as an explicit operator action, rather than at every
// startup: silently rewriting an operator's mailbox routing when a state file
// appears would be a surprising thing for a daemon to do. The prior routing
// comes back on disconnect.
func bindLocalInbox(ctx context.Context, cfg config.Config, address string) (*hybridtransport.LocalMailbox, error) {
	if strings.TrimSpace(address) == "" {
		return nil, errors.New("no local mailbox address; pass -local-inbox or set smtp.from")
	}
	st, err := store.Open(cfg.Database.DSN)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	inboxID, err := st.EnsureDefaults(ctx, address)
	if err != nil {
		return nil, err
	}
	record, err := st.GetInboxRecordByID(ctx, inboxID)
	if err != nil {
		return nil, err
	}
	mailbox := &hybridtransport.LocalMailbox{
		InboxID: inboxID, Address: address,
		PriorInbound: record.InboundProvider, PriorOutbound: record.OutboundProvider,
	}
	// Re-running connect must not record "hybrid" as the routing to restore.
	if mailbox.PriorInbound == hybridtransport.ProviderName {
		mailbox.PriorInbound = ""
	}
	if mailbox.PriorOutbound == hybridtransport.ProviderName {
		mailbox.PriorOutbound = ""
	}
	if err := st.UpdateInboxTransportProviders(ctx, inboxID, hybridtransport.ProviderName); err != nil {
		return nil, err
	}
	return mailbox, nil
}

// restoreLocalInbox puts a mailbox back on the providers it used before it
// was routed through Cloud.
//
// Leaving it on "hybrid" after disconnect is not inert: the poll loop stops
// with "unknown inbound provider" and every outbound row is requeued forever,
// so a disconnect would take the whole runtime down with it.
func restoreLocalInbox(ctx context.Context, cfg config.Config, mailbox hybridtransport.LocalMailbox) error {
	st, err := store.Open(cfg.Database.DSN)
	if err != nil {
		return err
	}
	defer st.Close()
	inbound, outbound := mailbox.PriorInbound, mailbox.PriorOutbound
	// A mailbox created by connect itself has no earlier routing to return
	// to. Leave it on providers the runtime always registers rather than on
	// one that disappears with the installation.
	if inbound == "" || inbound == hybridtransport.ProviderName {
		inbound = "jmap"
	}
	if outbound == "" || outbound == hybridtransport.ProviderName {
		outbound = "smtp"
	}
	return st.UpdateInboxProviders(ctx, mailbox.InboxID, inbound, outbound)
}

func hybridStatus(ctx context.Context, stateStore hybridtransport.Store, client *http.Client, out io.Writer) error {
	report, err := hybridtransport.Status(ctx, stateStore, client)
	if err != nil {
		return err
	}
	return writeJSONBlock(out, report)
}

func hybridRotate(ctx context.Context, cfg config.Config, stateStore hybridtransport.Store, client *http.Client, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("hybrid rotate", flag.ContinueOnError)
	flags.SetOutput(out)
	commit := flags.Bool("commit", false, "switch to the prepared key once Cloud accepts it")
	abandon := flags.Bool("abandon", false, "discard a prepared key that was never admitted")
	allowRunning := flags.Bool("allow-running", false, "proceed even though the runtime is serving; restart it immediately afterwards")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected hybrid rotate arguments")
	}
	switch {
	case *commit && *abandon:
		return errors.New("choose either -commit or -abandon")
	case *abandon:
		if err := hybridtransport.AbandonRotation(stateStore); err != nil {
			return err
		}
		fmt.Fprintln(out, "Discarded the prepared replacement key. The active key is unchanged.")
		return nil
	case *commit:
		// A serving daemon holds the old key in memory. Telling the operator
		// to remove it from Cloud while the runtime is still signing with it
		// would take the installation offline.
		if err := requireStoppedRuntime(ctx, cfg, *allowRunning, "rotating"); err != nil {
			return err
		}
		state, err := hybridtransport.CommitRotation(ctx, stateStore, client)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Now signing with key %s.\n", state.Key.KID)
		fmt.Fprintln(out, "Restart the runtime, confirm it is healthy, then remove the previous key from the Cloud inventory.")
		return nil
	}
	// An unconfirmed write still put the replacement on disk, so print it
	// either way: the operator has to admit that exact key, and a run that
	// showed nothing would leave them unable to.
	record, err := hybridtransport.PrepareRotation(stateStore)
	if err != nil && record.KeyID == "" {
		return err
	}
	fmt.Fprintf(out, "Prepared replacement key %s. The runtime keeps using its current key until you commit.\n", record.KeyID)
	fmt.Fprintln(out, "Admit this public key, have the owner rotate the installation onto it, then re-run with -commit:")
	if blockErr := writeJSONBlock(out, record); blockErr != nil {
		return blockErr
	}
	if err != nil {
		fmt.Fprintln(out, "The key is on disk but the filesystem would not confirm it survives a reboot.")
		fmt.Fprintln(out, "Check the disk, then re-run `hybrid rotate`: it rewrites the file and retries the sync.")
		return fmt.Errorf("replacement key %s is not confirmed durable: %w", record.KeyID, err)
	}
	return nil
}

func hybridDisconnect(ctx context.Context, cfg config.Config, stateStore hybridtransport.Store, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("hybrid disconnect", flag.ContinueOnError)
	flags.SetOutput(out)
	allowRunning := flags.Bool("allow-running", false, "proceed even though the runtime is serving; restart it immediately afterwards")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected hybrid disconnect arguments")
	}
	if err := requireStoppedRuntime(ctx, cfg, *allowRunning, "disconnecting"); err != nil {
		return err
	}
	// Put the mailbox back first. If removing the key then fails, the runtime
	// still works; the other order leaves a mailbox routed to a provider that
	// no longer exists, which stops the poll loop entirely.
	state, err := stateStore.Load()
	switch {
	case err == nil && state.LocalMailbox != nil:
		if err := restoreLocalInbox(ctx, cfg, *state.LocalMailbox); err != nil {
			return fmt.Errorf("local mailbox %s was not restored, so the runtime would stop polling: %w",
				state.LocalMailbox.Address, err)
		}
		fmt.Fprintf(out, "Local mailbox %s no longer routes through Cloud.\n", state.LocalMailbox.Address)
	case err != nil && !errors.Is(err, hybridtransport.ErrNotConnected):
		// An unreadable state file still has to be removable, but then
		// nothing is known about the routing to put back.
		fmt.Fprintln(out, "Installation state was unreadable; check the mailbox's providers by hand.")
	}
	if err := hybridtransport.Disconnect(stateStore); err != nil {
		return err
	}
	fmt.Fprintln(out, "Removed the local installation and its private key.")
	// Deleting the key locally does not stop anything at Cloud. Only the owner
	// can revoke, and an operator who thinks this command did it would leave a
	// live installation behind.
	fmt.Fprintln(out, "This did not revoke anything: ask the mailbox owner to revoke the installation in Cloud.")
	fmt.Fprintln(out, "Restart the runtime so it stops carrying mail for this installation.")
	return nil
}

func writeJSONBlock(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// hybridSignalContext cancels the long connect wait on Ctrl-C so an operator
// is never stuck watching a pairing they have given up on.
func hybridSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
