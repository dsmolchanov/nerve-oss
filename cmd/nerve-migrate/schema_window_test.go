package main

import (
	"context"
	"io"
	"neuralmail/internal/startup"
	"testing"
)

func TestSuccessorMigratorPolicy(t *testing.T) {
	old := startup.CompiledSchemaWindow
	t.Cleanup(func() { startup.CompiledSchemaWindow = old })
	startup.CompiledSchemaWindow = "2:29:30"
	for _, args := range [][]string{
		{"down", "--scope", "core", "--steps", "1"}, {"up"}, {"up", "--scope", "cloud"},
		{"up", "--scope", "core", "--to", "31"}, {"up", "--scope", "core", "--to", "28"},
	} {
		opened := false
		err := run(context.Background(), args, io.Discard, func() (migrationBackend, error) { opened = true; return nil, nil })
		if err == nil || opened {
			t.Fatalf("%v: err=%v opened=%t", args, err, opened)
		}
	}
	for _, args := range [][]string{{"up", "--scope", "core"}, {"up", "--scope", "core", "--to", "29"}, {"up", "--scope", "core", "--to", "30"}, {"status", "--scope", "core"}} {
		cmd, err := parseCommand(args)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateCompiledCommand(cmd); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if got := defaultTarget(scopeCore); got != 30 {
		t.Fatalf("target %d", got)
	}
	startup.CompiledSchemaWindow = "broken"
	if err := run(context.Background(), []string{"status", "--scope", "core"}, io.Discard, func() (migrationBackend, error) { t.Fatal("opened with malformed window"); return nil, nil }); err == nil {
		t.Fatal("malformed accepted")
	}
}

func TestSuccessorMigratorUsesCompiledTarget(t *testing.T) {
	old := startup.CompiledSchemaWindow
	t.Cleanup(func() { startup.CompiledSchemaWindow = old })
	startup.CompiledSchemaWindow = "2:29:30"
	backend := &fakeBackend{statuses: map[migrationScope][]statusResult{scopeCore: {
		{status: migrationStatus{Current: 29, Head: 30, Pending: []int64{30}}},
		{status: migrationStatus{Current: 30, Head: 30}},
	}}}
	if _, err := runFake(t, []string{"up", "--scope", "core"}, backend); err != nil {
		t.Fatal(err)
	}
	want := []string{"status:core", "up:core:30", "status:core", "close"}
	if len(backend.calls) != len(want) {
		t.Fatalf("calls: %v", backend.calls)
	}
	for i, call := range want {
		if backend.calls[i] != call {
			t.Fatalf("calls: %v", backend.calls)
		}
	}
}
