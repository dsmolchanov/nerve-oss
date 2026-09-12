package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
		return hybridRotate(ctx, stateStore, client, args[1:], out)
	case "disconnect":
		return hybridDisconnect(stateStore, out)
	default:
		return fmt.Errorf("unknown hybrid command %q", args[0])
	}
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
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected hybrid connect arguments")
	}

	// Begin only needs the parameters on a first run. A resumed connect reads
	// them back from the state file, so an operator re-running the command
	// after approving does not have to retype them.
	if _, err := stateStore.Load(); errors.Is(err, hybridtransport.ErrNotConnected) {
		params := hybridtransport.ConnectParams{
			CloudBaseURL: *cloudURL, TokenEndpoint: *tokenEndpoint, Resource: *resource,
			ClientID: *clientID, Generation: *generation, InboxID: *inboxID, AuthorityID: *authorityID,
		}
		state, err := hybridtransport.Begin(stateStore, params)
		if err != nil {
			return err
		}
		record, err := hybridtransport.Admission(state)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Generated installation key %s.\n", record.KeyID)
		fmt.Fprintln(out, "Admit this public key to the Cloud machine-client inventory before continuing:")
		if err := writeJSONBlock(out, record); err != nil {
			return err
		}
	} else if err != nil {
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
	if err := bindLocalInbox(ctx, cfg, address); err != nil {
		// The installation is real and recorded; only the local routing is
		// missing. Say so precisely rather than implying the pairing failed.
		return fmt.Errorf("installation %s is connected but local mailbox %s was not routed to it: %w",
			state.InstallationID, address, err)
	}
	fmt.Fprintf(out, "Local mailbox %s now sends and receives through Cloud.\n", address)
	return nil
}

// bindLocalInbox points one local mailbox at the hybrid transport. It is done
// here, as an explicit operator action, rather than at every startup: silently
// rewriting an operator's mailbox routing when a state file appears would be a
// surprising thing for a daemon to do.
func bindLocalInbox(ctx context.Context, cfg config.Config, address string) error {
	if strings.TrimSpace(address) == "" {
		return errors.New("no local mailbox address; pass -local-inbox or set smtp.from")
	}
	st, err := store.Open(cfg.Database.DSN)
	if err != nil {
		return err
	}
	defer st.Close()
	inboxID, err := st.EnsureDefaults(ctx, address)
	if err != nil {
		return err
	}
	return st.UpdateInboxTransportProviders(ctx, inboxID, hybridtransport.ProviderName)
}

func hybridStatus(ctx context.Context, stateStore hybridtransport.Store, client *http.Client, out io.Writer) error {
	report, err := hybridtransport.Status(ctx, stateStore, client)
	if err != nil {
		return err
	}
	return writeJSONBlock(out, report)
}

func hybridRotate(ctx context.Context, stateStore hybridtransport.Store, client *http.Client, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("hybrid rotate", flag.ContinueOnError)
	flags.SetOutput(out)
	commit := flags.Bool("commit", false, "switch to the prepared key once Cloud accepts it")
	abandon := flags.Bool("abandon", false, "discard a prepared key that was never admitted")
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
		state, err := hybridtransport.CommitRotation(ctx, stateStore, client)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Now signing with key %s. Remove the previous key from the Cloud inventory.\n", state.Key.KID)
		return nil
	}
	record, err := hybridtransport.PrepareRotation(stateStore)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Prepared replacement key %s. The runtime keeps using its current key until you commit.\n", record.KeyID)
	fmt.Fprintln(out, "Admit this public key, have the owner rotate the installation onto it, then re-run with -commit:")
	return writeJSONBlock(out, record)
}

func hybridDisconnect(stateStore hybridtransport.Store, out io.Writer) error {
	if err := hybridtransport.Disconnect(stateStore); err != nil {
		return err
	}
	fmt.Fprintln(out, "Removed the local installation and its private key.")
	// Deleting the key locally does not stop anything at Cloud. Only the owner
	// can revoke, and an operator who thinks this command did it would leave a
	// live installation behind.
	fmt.Fprintln(out, "This did not revoke anything: ask the mailbox owner to revoke the installation in Cloud.")
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
