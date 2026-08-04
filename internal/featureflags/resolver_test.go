package featureflags

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"neuralmail/internal/store"
)

type flagStoreStub struct {
	values store.FeatureFlagValues
	err    error
	calls  int
}

func (s *flagStoreStub) LookupFeatureFlagForOrg(_ context.Context, _ string, _ string) (store.FeatureFlagValues, error) {
	s.calls++
	return s.values, s.err
}

func TestResolverPrecedenceAllSourceStates(t *testing.T) {
	states := []optionalBool{{}, {set: true}, {set: true, value: true}}

	for _, env := range states {
		for _, org := range states {
			for _, global := range states {
				name := optionalName("env", env) + "/" + optionalName("org", org) + "/" + optionalName("global", global)
				t.Run(name, func(t *testing.T) {
					stub := &flagStoreStub{values: store.FeatureFlagValues{
						Org:    optionalPointer(org),
						Global: optionalPointer(global),
					}}
					resolver := testResolver(true, stub)
					resolver.lookupEnv = func(string) (string, bool) {
						if !env.set {
							return "", false
						}
						if env.value {
							return "force-on", true
						}
						return "force-off", true
					}

					got, err := resolver.Enabled(context.Background(), "attachments", "org-a")
					if err != nil {
						t.Fatal(err)
					}
					want := false
					switch {
					case env.set:
						want = env.value
					case org.set:
						want = org.value
					case global.set:
						want = global.value
					}
					if got != want {
						t.Fatalf("enabled=%t, want %t", got, want)
					}
					if env.set && stub.calls != 0 {
						t.Fatalf("env override queried store %d times", stub.calls)
					}
				})
			}
		}
	}
}

func TestResolverCachesPositiveNegativeAndErrors(t *testing.T) {
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		values store.FeatureFlagValues
		err    error
		want   bool
	}{
		{name: "positive", values: store.FeatureFlagValues{Org: boolPointer(true)}, want: true},
		{name: "negative", values: store.FeatureFlagValues{Org: boolPointer(false)}},
		{name: "missing"},
		{name: "lookup error", err: errors.New("database unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &flagStoreStub{values: test.values, err: test.err}
			resolver := testResolver(true, stub)
			resolver.now = func() time.Time { return now }
			resolver.ttl = 30 * time.Second

			for index := 0; index < 2; index++ {
				got, err := resolver.Enabled(context.Background(), "attachments", "org-a")
				if err != nil || got != test.want {
					t.Fatalf("call %d enabled=%t err=%v, want %t", index, got, err, test.want)
				}
			}
			if stub.calls != 1 {
				t.Fatalf("cached calls=%d, want 1", stub.calls)
			}

			now = now.Add(31 * time.Second)
			_, _ = resolver.Enabled(context.Background(), "attachments", "org-a")
			if stub.calls != 2 {
				t.Fatalf("post-TTL calls=%d, want 2", stub.calls)
			}
		})
	}
}

func TestResolverOSSLocalNeverQueriesStore(t *testing.T) {
	stub := &flagStoreStub{values: store.FeatureFlagValues{Global: boolPointer(true)}}
	resolver := testResolver(false, stub)

	got, err := resolver.Enabled(context.Background(), "attachments", "")
	if err != nil || got {
		t.Fatalf("local default enabled=%t err=%v", got, err)
	}
	if stub.calls != 0 {
		t.Fatalf("local resolver queried store %d times", stub.calls)
	}

	resolver.lookupEnv = func(string) (string, bool) { return "force-on", true }
	got, err = resolver.Enabled(context.Background(), "attachments", "")
	if err != nil || !got {
		t.Fatalf("local env override enabled=%t err=%v", got, err)
	}
	if stub.calls != 0 {
		t.Fatalf("local env override queried store %d times", stub.calls)
	}
}

func TestResolverInvalidOverrideFailsClosed(t *testing.T) {
	stub := &flagStoreStub{values: store.FeatureFlagValues{Org: boolPointer(true)}}
	resolver := testResolver(true, stub)
	resolver.lookupEnv = func(string) (string, bool) { return "yes", true }

	got, err := resolver.Enabled(context.Background(), "attachments", "org-a")
	if err != nil || got {
		t.Fatalf("invalid override enabled=%t err=%v", got, err)
	}
	if stub.calls != 0 {
		t.Fatalf("invalid override queried store %d times", stub.calls)
	}
}

func testResolver(cloudMode bool, flagStore Store) *Resolver {
	resolver := New(cloudMode, flagStore)
	resolver.lookupEnv = func(string) (string, bool) { return "", false }
	resolver.logger = log.New(io.Discard, "", 0)
	return resolver
}

func optionalPointer(value optionalBool) *bool {
	if !value.set {
		return nil
	}
	return boolPointer(value.value)
}

type optionalBool struct {
	set   bool
	value bool
}

func optionalName(prefix string, value optionalBool) string {
	if !value.set {
		return prefix + "-unset"
	}
	if value.value {
		return prefix + "-on"
	}
	return prefix + "-off"
}

func boolPointer(value bool) *bool {
	return &value
}
