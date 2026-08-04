package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseCommandValid(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantAction action
		wantScope  migrationScope
		wantTarget *int64
		wantHelp   bool
	}{
		{name: "global long help", args: []string{"--help"}, wantHelp: true},
		{name: "global short help", args: []string{"-h"}, wantHelp: true},
		{name: "subcommand help", args: []string{"up", "--help"}, wantHelp: true},
		{name: "up defaults to all", args: []string{"up"}, wantAction: actionUp, wantScope: scopeAll},
		{name: "up core", args: []string{"up", "--scope", "core"}, wantAction: actionUp, wantScope: scopeCore},
		{name: "up cloud target with leading zeroes", args: []string{"up", "--scope=cloud", "--to=0018"}, wantAction: actionUp, wantScope: scopeCloud, wantTarget: int64Ptr(18)},
		{name: "up zero target", args: []string{"up", "--to", "0"}, wantAction: actionUp, wantScope: scopeAll, wantTarget: int64Ptr(0)},
		{name: "status defaults to all", args: []string{"status"}, wantAction: actionStatus, wantScope: scopeAll},
		{name: "status core", args: []string{"status", "--scope", "core"}, wantAction: actionStatus, wantScope: scopeCore},
		{name: "status cloud equals", args: []string{"status", "--scope=cloud"}, wantAction: actionStatus, wantScope: scopeCloud},
		{name: "down one core step", args: []string{"down", "--scope", "core", "--steps", "1"}, wantAction: actionDown, wantScope: scopeCore},
		{name: "down one padded core step", args: []string{"down", "--steps=01", "--scope=core"}, wantAction: actionDown, wantScope: scopeCore},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCommand(tc.args)
			if err != nil {
				t.Fatalf("parseCommand() error = %v", err)
			}
			if got.action != tc.wantAction || got.scope != tc.wantScope || got.help != tc.wantHelp {
				t.Fatalf("parseCommand() = %#v, want action=%q scope=%q help=%v", got, tc.wantAction, tc.wantScope, tc.wantHelp)
			}
			if !equalInt64Pointers(got.target, tc.wantTarget) {
				t.Fatalf("target = %v, want %v", pointerText(got.target), pointerText(tc.wantTarget))
			}
		})
	}
}

func TestParseCommandRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing command"},
		{name: "unknown command", args: []string{"migrate"}},
		{name: "unsupported help command", args: []string{"help"}},
		{name: "flag before command", args: []string{"--scope", "core", "status"}},
		{name: "positional extra", args: []string{"status", "extra"}},
		{name: "bare separator", args: []string{"status", "--"}},
		{name: "unknown flag", args: []string{"status", "--wat", "value"}},
		{name: "dsn flag", args: []string{"up", "--dsn", "postgres://example"}},
		{name: "missing scope value", args: []string{"status", "--scope"}},
		{name: "empty scope value", args: []string{"status", "--scope="}},
		{name: "invalid scope", args: []string{"status", "--scope", "CORE"}},
		{name: "duplicate scope", args: []string{"status", "--scope", "core", "--scope", "cloud"}},
		{name: "duplicate target", args: []string{"up", "--to", "1", "--to", "2"}},
		{name: "target missing", args: []string{"up", "--to"}},
		{name: "target empty", args: []string{"up", "--to="}},
		{name: "target plus sign", args: []string{"up", "--to", "+1"}},
		{name: "target minus sign", args: []string{"up", "--to", "-1"}},
		{name: "target leading space", args: []string{"up", "--to", " 1"}},
		{name: "target trailing space", args: []string{"up", "--to", "1 "}},
		{name: "target unicode digit", args: []string{"up", "--to", "١"}},
		{name: "target overflow", args: []string{"up", "--to", "9223372036854775808"}},
		{name: "steps on up", args: []string{"up", "--steps", "1"}},
		{name: "target on status", args: []string{"status", "--to", "1"}},
		{name: "steps on status", args: []string{"status", "--steps", "1"}},
		{name: "down missing scope", args: []string{"down", "--steps", "1"}},
		{name: "down all scope", args: []string{"down", "--scope", "all", "--steps", "1"}},
		{name: "down cloud scope", args: []string{"down", "--scope", "cloud", "--steps", "1"}},
		{name: "down missing steps", args: []string{"down", "--scope", "core"}},
		{name: "down zero steps", args: []string{"down", "--scope", "core", "--steps", "0"}},
		{name: "down multiple steps", args: []string{"down", "--scope", "core", "--steps", "2"}},
		{name: "down signed steps", args: []string{"down", "--scope", "core", "--steps", "+1"}},
		{name: "down target", args: []string{"down", "--scope", "core", "--steps", "1", "--to", "1"}},
		{name: "help with extra input", args: []string{"up", "--help", "--dsn", "x"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseCommand(tc.args); err == nil {
				t.Fatal("parseCommand() error = nil, want rejection")
			}
		})
	}
}

func TestRunHelpAndParseErrorsDoNotOpenBackend(t *testing.T) {
	t.Run("help", func(t *testing.T) {
		var output bytes.Buffer
		openCalls := 0
		err := run(context.Background(), []string{"--help"}, &output, func() (migrationBackend, error) {
			openCalls++
			return nil, errors.New("must not open")
		})
		if err != nil {
			t.Fatalf("run() error = %v", err)
		}
		if openCalls != 0 {
			t.Fatalf("open calls = %d, want 0", openCalls)
		}
		if output.String() != usageText {
			t.Fatalf("output = %q, want usage", output.String())
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		var output bytes.Buffer
		openCalls := 0
		err := run(context.Background(), []string{"up", "--dsn", "postgres://example"}, &output, func() (migrationBackend, error) {
			openCalls++
			return nil, errors.New("must not open")
		})
		if err == nil {
			t.Fatal("run() error = nil, want parse error")
		}
		if openCalls != 0 {
			t.Fatalf("open calls = %d, want 0", openCalls)
		}
		if output.Len() != 0 {
			t.Fatalf("output = %q, want empty", output.String())
		}
	})
}

func TestRunBoundedAllPreflightsBothScopesAndRunsCoreFirst(t *testing.T) {
	backend := &fakeBackend{statuses: map[migrationScope][]statusResult{
		scopeCore: {
			{status: migrationStatus{Current: 1, Head: 3, Pending: []int64{2, 3}}},
			{status: migrationStatus{Current: 3, Head: 3}},
		},
		scopeCloud: {
			{status: migrationStatus{Current: 1, Head: 3, Pending: []int64{3}}},
			{status: migrationStatus{Current: 3, Head: 3}},
		},
	}}

	output, err := runFake(t, []string{"up", "--scope", "all", "--to", "0003"}, backend)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	wantCalls := []string{
		"status:core",
		"status:cloud",
		"up:core:3",
		"up:cloud:3",
		"status:core",
		"status:cloud",
		"close",
	}
	if !reflect.DeepEqual(backend.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", backend.calls, wantCalls)
	}
	wantOutput := "core current=0003 head=0003 pending=0 pending_versions=[]\n" +
		"cloud current=0003 head=0003 pending=0 pending_versions=[]\n"
	if output != wantOutput {
		t.Fatalf("output = %q, want %q", output, wantOutput)
	}
}

func TestRunBoundedAllCloudGapPreventsEveryMutation(t *testing.T) {
	backend := &fakeBackend{statuses: map[migrationScope][]statusResult{
		scopeCore:  {{status: migrationStatus{Current: 1, Head: 3, Pending: []int64{2, 3}}}},
		scopeCloud: {{status: migrationStatus{Current: 1, Head: 5, Pending: []int64{5}}}},
	}}

	output, err := runFake(t, []string{"up", "--to", "3"}, backend)
	if err == nil || !strings.Contains(err.Error(), "cloud migration target 3") {
		t.Fatalf("run() error = %v, want cloud target error", err)
	}
	if output != "" {
		t.Fatalf("output = %q, want empty", output)
	}
	wantCalls := []string{"status:core", "status:cloud", "close"}
	if !reflect.DeepEqual(backend.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", backend.calls, wantCalls)
	}
}

func TestRunUnboundedAllPreflightsBeforeCoreThenCloud(t *testing.T) {
	backend := &fakeBackend{statuses: map[migrationScope][]statusResult{
		scopeCore: {
			{status: migrationStatus{Current: 1, Head: 2, Pending: []int64{2}}},
			{status: migrationStatus{Current: 2, Head: 2}},
		},
		scopeCloud: {
			{status: migrationStatus{Current: 0, Head: 3, Pending: []int64{1, 3}}},
			{status: migrationStatus{Current: 3, Head: 3}},
		},
	}}

	_, err := runFake(t, []string{"up"}, backend)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	wantCalls := []string{
		"status:core",
		"status:cloud",
		"up:core:head",
		"up:cloud:head",
		"status:core",
		"status:cloud",
		"close",
	}
	if !reflect.DeepEqual(backend.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", backend.calls, wantCalls)
	}
}

func TestRunTargetAlreadyCurrentIsNoOp(t *testing.T) {
	backend := &fakeBackend{statuses: map[migrationScope][]statusResult{
		scopeCore: {
			{status: migrationStatus{Current: 5, Head: 8, Pending: []int64{8}}},
			{status: migrationStatus{Current: 5, Head: 8, Pending: []int64{8}}},
		},
	}}

	_, err := runFake(t, []string{"up", "--scope", "core", "--to", "5"}, backend)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	wantCalls := []string{"status:core", "status:core", "close"}
	if !reflect.DeepEqual(backend.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", backend.calls, wantCalls)
	}
}

func TestRunZeroTargetOnlyWorksForFreshScope(t *testing.T) {
	t.Run("fresh no-op", func(t *testing.T) {
		backend := &fakeBackend{statuses: map[migrationScope][]statusResult{
			scopeCore: {
				{status: migrationStatus{Current: 0, Head: 10, Pending: []int64{1, 2, 10}}},
				{status: migrationStatus{Current: 0, Head: 10, Pending: []int64{1, 2, 10}}},
			},
		}}
		if _, err := runFake(t, []string{"up", "--scope", "core", "--to", "0"}, backend); err != nil {
			t.Fatalf("run() error = %v", err)
		}
		wantCalls := []string{"status:core", "status:core", "close"}
		if !reflect.DeepEqual(backend.calls, wantCalls) {
			t.Fatalf("calls = %#v, want %#v", backend.calls, wantCalls)
		}
	})

	t.Run("applied rejects", func(t *testing.T) {
		backend := &fakeBackend{statuses: map[migrationScope][]statusResult{
			scopeCore: {{status: migrationStatus{Current: 1, Head: 10, Pending: []int64{2, 10}}}},
		}}
		output, err := runFake(t, []string{"up", "--scope", "core", "--to", "0"}, backend)
		if err == nil {
			t.Fatal("run() error = nil, want rejected target")
		}
		if output != "" {
			t.Fatalf("output = %q, want empty", output)
		}
		wantCalls := []string{"status:core", "close"}
		if !reflect.DeepEqual(backend.calls, wantCalls) {
			t.Fatalf("calls = %#v, want %#v", backend.calls, wantCalls)
		}
	})
}

func TestRunDownPreflightsAndRollsBackExactlyOneCoreStep(t *testing.T) {
	backend := &fakeBackend{statuses: map[migrationScope][]statusResult{
		scopeCore: {
			{status: migrationStatus{Current: 10, Head: 10}},
			{status: migrationStatus{Current: 9, Head: 10, Pending: []int64{10}}},
		},
	}}

	output, err := runFake(t, []string{"down", "--scope", "core", "--steps", "1"}, backend)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	wantCalls := []string{"status:core", "down:core", "status:core", "close"}
	if !reflect.DeepEqual(backend.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", backend.calls, wantCalls)
	}
	wantOutput := "core current=0009 head=0010 pending=1 pending_versions=[0010]\n"
	if output != wantOutput {
		t.Fatalf("output = %q, want %q", output, wantOutput)
	}
}

func TestRunDownFreshCoreDoesNotMutate(t *testing.T) {
	backend := &fakeBackend{statuses: map[migrationScope][]statusResult{
		scopeCore: {{status: migrationStatus{Current: 0, Head: 10, Pending: []int64{1, 10}}}},
	}}
	output, err := runFake(t, []string{"down", "--scope", "core", "--steps", "1"}, backend)
	if err == nil || !strings.Contains(err.Error(), "no applied migration") {
		t.Fatalf("run() error = %v, want fresh rollback error", err)
	}
	if output != "" {
		t.Fatalf("output = %q, want empty", output)
	}
	wantCalls := []string{"status:core", "close"}
	if !reflect.DeepEqual(backend.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", backend.calls, wantCalls)
	}
}

func TestRunStatusBuffersUntilEverySelectedScopeSucceeds(t *testing.T) {
	statusErr := errors.New("cloud status failed")
	backend := &fakeBackend{statuses: map[migrationScope][]statusResult{
		scopeCore:  {{status: migrationStatus{Current: 1, Head: 1}}},
		scopeCloud: {{err: statusErr}},
	}}

	output, err := runFake(t, []string{"status"}, backend)
	if !errors.Is(err, statusErr) {
		t.Fatalf("run() error = %v, want %v", err, statusErr)
	}
	if output != "" {
		t.Fatalf("output = %q, want empty", output)
	}
	wantCalls := []string{"status:core", "status:cloud", "close"}
	if !reflect.DeepEqual(backend.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", backend.calls, wantCalls)
	}
}

func TestRunFormatsSparseAndEmptyPendingVersionsStably(t *testing.T) {
	backend := &fakeBackend{statuses: map[migrationScope][]statusResult{
		scopeCore:  {{status: migrationStatus{Current: 5, Head: 10002, Pending: []int64{10002, 6, 10}}}},
		scopeCloud: {{status: migrationStatus{Current: 3, Head: 3}}},
	}}

	output, err := runFake(t, []string{"status"}, backend)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	wantOutput := "core current=0005 head=10002 pending=3 pending_versions=[0006,0010,10002]\n" +
		"cloud current=0003 head=0003 pending=0 pending_versions=[]\n"
	if output != wantOutput {
		t.Fatalf("output = %q, want %q", output, wantOutput)
	}
}

func TestRunClosesBackendOnOperationAndOpenErrors(t *testing.T) {
	t.Run("operation and close errors are both returned", func(t *testing.T) {
		operationErr := errors.New("migration failed")
		closeErr := errors.New("close failed")
		backend := &fakeBackend{
			statuses: map[migrationScope][]statusResult{
				scopeCore: {{status: migrationStatus{Current: 1, Head: 2, Pending: []int64{2}}}},
			},
			upErrors:   map[migrationScope]error{scopeCore: operationErr},
			closeError: closeErr,
		}
		output, err := runFake(t, []string{"up", "--scope", "core"}, backend)
		if !errors.Is(err, operationErr) || !errors.Is(err, closeErr) {
			t.Fatalf("run() error = %v, want joined operation and close errors", err)
		}
		if output != "" {
			t.Fatalf("output = %q, want empty", output)
		}
		if backend.closeCalls != 1 {
			t.Fatalf("close calls = %d, want 1", backend.closeCalls)
		}
	})

	t.Run("partially opened backend is closed", func(t *testing.T) {
		openErr := errors.New("open failed")
		backend := &fakeBackend{}
		var output bytes.Buffer
		err := run(context.Background(), []string{"status"}, &output, func() (migrationBackend, error) {
			return backend, openErr
		})
		if !errors.Is(err, openErr) {
			t.Fatalf("run() error = %v, want %v", err, openErr)
		}
		if backend.closeCalls != 1 {
			t.Fatalf("close calls = %d, want 1", backend.closeCalls)
		}
		if output.Len() != 0 {
			t.Fatalf("output = %q, want empty", output.String())
		}
	})
}

func TestRunCloseFailureSuppressesSuccessfulStatusOutput(t *testing.T) {
	closeErr := errors.New("close failed")
	backend := &fakeBackend{
		statuses:   map[migrationScope][]statusResult{scopeCore: {{status: migrationStatus{Current: 1, Head: 1}}}},
		closeError: closeErr,
	}
	output, err := runFake(t, []string{"status", "--scope", "core"}, backend)
	if !errors.Is(err, closeErr) {
		t.Fatalf("run() error = %v, want %v", err, closeErr)
	}
	if output != "" {
		t.Fatalf("output = %q, want empty", output)
	}
}

type statusResult struct {
	status migrationStatus
	err    error
}

type fakeBackend struct {
	statuses    map[migrationScope][]statusResult
	statusIndex map[migrationScope]int
	upErrors    map[migrationScope]error
	downErrors  map[migrationScope]error
	closeError  error
	closeCalls  int
	calls       []string
}

func (b *fakeBackend) Status(_ context.Context, scope migrationScope) (migrationStatus, error) {
	b.calls = append(b.calls, "status:"+string(scope))
	if b.statusIndex == nil {
		b.statusIndex = make(map[migrationScope]int)
	}
	index := b.statusIndex[scope]
	b.statusIndex[scope]++
	results := b.statuses[scope]
	if index >= len(results) {
		return migrationStatus{}, fmt.Errorf("no scripted %s status at index %d", scope, index)
	}
	return results[index].status, results[index].err
}

func (b *fakeBackend) Up(_ context.Context, scope migrationScope, target *int64) error {
	label := "head"
	if target != nil {
		label = strconvFormatInt(*target)
	}
	b.calls = append(b.calls, "up:"+string(scope)+":"+label)
	return b.upErrors[scope]
}

func (b *fakeBackend) Down(_ context.Context, scope migrationScope) error {
	b.calls = append(b.calls, "down:"+string(scope))
	return b.downErrors[scope]
}

func (b *fakeBackend) Close() error {
	b.calls = append(b.calls, "close")
	b.closeCalls++
	return b.closeError
}

func runFake(t *testing.T, args []string, backend migrationBackend) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := run(context.Background(), args, &output, func() (migrationBackend, error) {
		return backend, nil
	})
	return output.String(), err
}

func int64Ptr(value int64) *int64 {
	return &value
}

func equalInt64Pointers(left, right *int64) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func pointerText(value *int64) string {
	if value == nil {
		return "<nil>"
	}
	return strconvFormatInt(*value)
}

func strconvFormatInt(value int64) string {
	return fmt.Sprintf("%d", value)
}
