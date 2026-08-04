package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

const usageText = `Usage:
  nerve-migrate up     [--scope core|cloud|all] [--to <version>]
  nerve-migrate down   --scope core --steps 1
  nerve-migrate status [--scope core|cloud|all]
`

type action string

const (
	actionUp     action = "up"
	actionDown   action = "down"
	actionStatus action = "status"
)

type migrationScope string

const (
	scopeCore  migrationScope = "core"
	scopeCloud migrationScope = "cloud"
	scopeAll   migrationScope = "all"
)

type command struct {
	action action
	scope  migrationScope
	target *int64
	help   bool
}

type migrationStatus struct {
	Current int64
	Head    int64
	Pending []int64
}

type migrationBackend interface {
	Status(context.Context, migrationScope) (migrationStatus, error)
	Up(context.Context, migrationScope, *int64) error
	Down(context.Context, migrationScope) error
	Close() error
}

type openBackendFunc func() (migrationBackend, error)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, os.Args[1:], os.Stdout, openStoreBackend); err != nil {
		fmt.Fprintf(os.Stderr, "nerve-migrate: %v\n", err)
		os.Exit(1)
	}
}

// run parses the complete command before invoking openBackend. Keeping parsing
// on this side of the dependency boundary makes help and invalid input safe to
// use without configuration or database access.
func run(ctx context.Context, args []string, stdout io.Writer, openBackend openBackendFunc) error {
	cmd, err := parseCommand(args)
	if err != nil {
		return err
	}
	if cmd.help {
		_, err := io.WriteString(stdout, usageText)
		return err
	}

	backend, err := openBackend()
	if err != nil {
		if backend != nil {
			if closeErr := backend.Close(); closeErr != nil {
				return errors.Join(err, fmt.Errorf("close migration backend: %w", closeErr))
			}
		}
		return err
	}
	if backend == nil {
		return errors.New("open migration backend: returned nil backend")
	}

	output, commandErr := execute(ctx, cmd, backend)
	closeErr := backend.Close()
	if commandErr != nil {
		if closeErr != nil {
			return errors.Join(commandErr, fmt.Errorf("close migration backend: %w", closeErr))
		}
		return commandErr
	}
	if closeErr != nil {
		return fmt.Errorf("close migration backend: %w", closeErr)
	}

	if _, err := io.WriteString(stdout, output); err != nil {
		return fmt.Errorf("write migration status: %w", err)
	}
	return nil
}

func parseCommand(args []string) (command, error) {
	if len(args) == 1 && isHelp(args[0]) {
		return command{help: true}, nil
	}
	if len(args) == 2 && isAction(args[0]) && isHelp(args[1]) {
		return command{help: true}, nil
	}
	if len(args) == 0 {
		return command{}, errors.New("missing command (expected up, down, or status)")
	}

	cmd := command{action: action(args[0]), scope: scopeAll}
	if !isAction(args[0]) {
		return command{}, fmt.Errorf("unknown command %q (expected up, down, or status)", args[0])
	}

	var (
		scopeValue string
		targetText string
		stepsText  string
		scopeSet   bool
		targetSet  bool
		stepsSet   bool
	)
	for i := 1; i < len(args); i++ {
		name, value, hasValue, err := splitFlag(args, &i)
		if err != nil {
			return command{}, err
		}
		switch name {
		case "scope":
			if scopeSet {
				return command{}, errors.New("--scope may be specified only once")
			}
			scopeSet = true
			scopeValue = value
		case "to":
			if targetSet {
				return command{}, errors.New("--to may be specified only once")
			}
			targetSet = true
			targetText = value
		case "steps":
			if stepsSet {
				return command{}, errors.New("--steps may be specified only once")
			}
			stepsSet = true
			stepsText = value
		default:
			return command{}, fmt.Errorf("unknown flag --%s", name)
		}
		if !hasValue {
			return command{}, fmt.Errorf("--%s requires a value", name)
		}
	}

	if scopeSet {
		scope, err := parseScope(scopeValue)
		if err != nil {
			return command{}, err
		}
		cmd.scope = scope
	}

	switch cmd.action {
	case actionUp:
		if stepsSet {
			return command{}, errors.New("--steps is valid only with down")
		}
		if targetSet {
			target, err := parseDecimal("migration target", targetText)
			if err != nil {
				return command{}, err
			}
			cmd.target = &target
		}
	case actionStatus:
		if targetSet {
			return command{}, errors.New("--to is valid only with up")
		}
		if stepsSet {
			return command{}, errors.New("--steps is valid only with down")
		}
	case actionDown:
		if targetSet {
			return command{}, errors.New("--to is valid only with up")
		}
		if !scopeSet || cmd.scope != scopeCore {
			return command{}, errors.New("down requires explicit --scope core")
		}
		if !stepsSet {
			return command{}, errors.New("down requires --steps 1")
		}
		steps, err := parseDecimal("rollback steps", stepsText)
		if err != nil {
			return command{}, err
		}
		if steps != 1 {
			return command{}, errors.New("down supports exactly one step (--steps 1)")
		}
	}

	return cmd, nil
}

func splitFlag(args []string, index *int) (name, value string, hasValue bool, err error) {
	arg := args[*index]
	if !strings.HasPrefix(arg, "--") || arg == "--" {
		return "", "", false, fmt.Errorf("unexpected argument %q", arg)
	}
	flagText := strings.TrimPrefix(arg, "--")
	if flagText == "" {
		return "", "", false, fmt.Errorf("unexpected argument %q", arg)
	}
	if name, value, found := strings.Cut(flagText, "="); found {
		if name == "" {
			return "", "", false, fmt.Errorf("unexpected argument %q", arg)
		}
		return name, value, value != "", nil
	}

	name = flagText
	if *index+1 >= len(args) || strings.HasPrefix(args[*index+1], "--") {
		return name, "", false, nil
	}
	*index++
	return name, args[*index], true, nil
}

func parseScope(value string) (migrationScope, error) {
	switch migrationScope(value) {
	case scopeCore, scopeCloud, scopeAll:
		return migrationScope(value), nil
	default:
		return "", fmt.Errorf("invalid scope %q (expected core, cloud, or all)", value)
	}
}

func parseDecimal(label, value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("%s must be non-empty ASCII decimal digits", label)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, fmt.Errorf("%s %q must contain only ASCII decimal digits", label, value)
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 63)
	if err != nil {
		return 0, fmt.Errorf("%s %q is out of range", label, value)
	}
	return int64(parsed), nil
}

func isAction(value string) bool {
	switch action(value) {
	case actionUp, actionDown, actionStatus:
		return true
	default:
		return false
	}
}

func isHelp(value string) bool {
	return value == "--help" || value == "-h"
}

func execute(ctx context.Context, cmd command, backend migrationBackend) (string, error) {
	switch cmd.action {
	case actionStatus:
		statuses, err := readStatuses(ctx, backend, cmd.scope)
		if err != nil {
			return "", err
		}
		return formatStatuses(statuses), nil
	case actionUp:
		initial, err := readStatuses(ctx, backend, cmd.scope)
		if err != nil {
			return "", err
		}
		if cmd.target != nil {
			for _, scoped := range initial {
				if err := validateTarget(scoped.scope, scoped.status, *cmd.target); err != nil {
					return "", err
				}
			}
		}

		for _, scoped := range initial {
			if cmd.target != nil && scoped.status.Current == *cmd.target {
				continue
			}
			if err := backend.Up(ctx, scoped.scope, cmd.target); err != nil {
				if cmd.target == nil {
					return "", fmt.Errorf("migrate %s: %w", scoped.scope, err)
				}
				return "", fmt.Errorf("migrate %s to %d: %w", scoped.scope, *cmd.target, err)
			}
		}

		statuses, err := readStatuses(ctx, backend, cmd.scope)
		if err != nil {
			return "", err
		}
		return formatStatuses(statuses), nil
	case actionDown:
		initial, err := readStatuses(ctx, backend, scopeCore)
		if err != nil {
			return "", err
		}
		if initial[0].status.Current == 0 {
			return "", errors.New("core has no applied migration to roll back")
		}
		if err := backend.Down(ctx, scopeCore); err != nil {
			return "", fmt.Errorf("roll back core migration: %w", err)
		}
		statuses, err := readStatuses(ctx, backend, scopeCore)
		if err != nil {
			return "", err
		}
		return formatStatuses(statuses), nil
	default:
		return "", fmt.Errorf("unsupported command %q", cmd.action)
	}
}

type scopedStatus struct {
	scope  migrationScope
	status migrationStatus
}

func readStatuses(ctx context.Context, backend migrationBackend, scope migrationScope) ([]scopedStatus, error) {
	scopes, err := expandScopes(scope)
	if err != nil {
		return nil, err
	}
	statuses := make([]scopedStatus, 0, len(scopes))
	for _, selected := range scopes {
		status, err := backend.Status(ctx, selected)
		if err != nil {
			return nil, fmt.Errorf("read %s migration status: %w", selected, err)
		}
		statuses = append(statuses, scopedStatus{scope: selected, status: status})
	}
	return statuses, nil
}

func expandScopes(scope migrationScope) ([]migrationScope, error) {
	switch scope {
	case scopeCore:
		return []migrationScope{scopeCore}, nil
	case scopeCloud:
		return []migrationScope{scopeCloud}, nil
	case scopeAll:
		return []migrationScope{scopeCore, scopeCloud}, nil
	default:
		return nil, fmt.Errorf("unsupported migration scope %q", scope)
	}
}

func validateTarget(scope migrationScope, status migrationStatus, target int64) error {
	if status.Current == target {
		return nil
	}
	for _, pending := range status.Pending {
		if pending == target {
			return nil
		}
	}
	return fmt.Errorf(
		"%s migration target %d is neither current (%d) nor pending (%s)",
		scope,
		target,
		status.Current,
		formatVersions(status.Pending),
	)
}

func formatStatuses(statuses []scopedStatus) string {
	var output strings.Builder
	for _, scoped := range statuses {
		pending := append([]int64(nil), scoped.status.Pending...)
		sort.Slice(pending, func(i, j int) bool { return pending[i] < pending[j] })
		fmt.Fprintf(
			&output,
			"%s current=%04d head=%04d pending=%d pending_versions=%s\n",
			scoped.scope,
			scoped.status.Current,
			scoped.status.Head,
			len(pending),
			formatVersions(pending),
		)
	}
	return output.String()
}

func formatVersions(versions []int64) string {
	if len(versions) == 0 {
		return "[]"
	}
	formatted := make([]string, len(versions))
	for i, version := range versions {
		formatted[i] = fmt.Sprintf("%04d", version)
	}
	return "[" + strings.Join(formatted, ",") + "]"
}

type storeBackend struct {
	store *store.Store
}

func openStoreBackend() (migrationBackend, error) {
	cfg, err := config.Load(config.ConfigPathFromEnv())
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	storeInstance, err := store.Open(cfg.Database.DSN)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return &storeBackend{store: storeInstance}, nil
}

func (b *storeBackend) Status(ctx context.Context, scope migrationScope) (migrationStatus, error) {
	var (
		status store.MigrationStatus
		err    error
	)
	switch scope {
	case scopeCore:
		status, err = store.MigrationStatusCore(ctx, b.store.DB())
	case scopeCloud:
		status, err = store.MigrationStatusCloud(ctx, b.store.DB())
	default:
		return migrationStatus{}, fmt.Errorf("unsupported migration scope %q", scope)
	}
	if err != nil {
		return migrationStatus{}, err
	}
	return migrationStatus{
		Current: status.Current,
		Head:    status.Head,
		Pending: append([]int64(nil), status.Pending...),
	}, nil
}

func (b *storeBackend) Up(ctx context.Context, scope migrationScope, target *int64) error {
	switch scope {
	case scopeCore:
		if target != nil {
			return store.MigrateUpToCore(ctx, b.store.DB(), *target)
		}
		return store.MigrateCore(ctx, b.store.DB())
	case scopeCloud:
		if target != nil {
			return store.MigrateUpToCloud(ctx, b.store.DB(), *target)
		}
		return store.MigrateCloud(ctx, b.store.DB())
	default:
		return fmt.Errorf("unsupported migration scope %q", scope)
	}
}

func (b *storeBackend) Down(ctx context.Context, scope migrationScope) error {
	if scope != scopeCore {
		return fmt.Errorf("unsupported rollback scope %q", scope)
	}
	return store.MigrateDownCore(ctx, b.store.DB())
}

func (b *storeBackend) Close() error {
	return b.store.Close()
}
