package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

func TestFetchChangesUsesQueryStateFamily(t *testing.T) {
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, args map[string]any) {
		if method != "Email/queryChanges" {
			t.Errorf("expected Email/queryChanges, got %q", method)
		}
		if got := getString(args, "sinceQueryState"); got != "query-state-1" {
			t.Errorf("expected sinceQueryState query-state-1, got %q", got)
		}
		if _, ok := args["sinceState"]; ok {
			t.Error("queryChanges request must not use Email/changes sinceState")
		}
		if _, ok := args["maxChanges"]; ok {
			t.Error("queryChanges request must not cap changes without paging")
		}
		writeJMAPTestResponse(t, w, method, map[string]any{
			"oldQueryState": "query-state-1",
			"newQueryState": "query-state-2",
			"added":         []any{},
		})
	})

	_, state, err := client.FetchChanges(context.Background(), "query-state-1")
	if err != nil {
		t.Fatalf("fetch changes: %v", err)
	}
	if state != "query-state-2" {
		t.Fatalf("expected new query state, got %q", state)
	}
}

func TestFetchChangesAcceptsNullAddedAsEmpty(t *testing.T) {
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
		writeJMAPTestResponse(t, w, method, map[string]any{
			"oldQueryState": "query-state-1",
			"newQueryState": "query-state-2",
			"added":         nil,
		})
	})

	emails, state, err := client.FetchChanges(context.Background(), "query-state-1")
	if err != nil {
		t.Fatalf("fetch changes: %v", err)
	}
	if len(emails) != 0 || state != "query-state-2" {
		t.Fatalf("emails/state = %#v/%q, want none/query-state-2", emails, state)
	}
}

func TestFetchChangesRecoversInvalidQueryCheckpoint(t *testing.T) {
	var methods []string
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, args map[string]any) {
		methods = append(methods, method)
		switch method {
		case "Email/queryChanges":
			if got := getString(args, "sinceQueryState"); got != "expired-state" {
				t.Errorf("sinceQueryState = %q, want expired-state", got)
			}
			writeJMAPTestResponse(t, w, "error", map[string]any{
				"type":        "cannotCalculateChanges",
				"description": "checkpoint expired",
			})
		case "Email/query":
			if got := args["limit"]; got != float64(recoveryQueryPageSize) {
				t.Errorf("recovery query limit = %#v, want %d", got, recoveryQueryPageSize)
			}
			if calculateTotal, _ := args["calculateTotal"].(bool); !calculateTotal {
				t.Error("recovery query must calculate total")
			}
			if got := args["filter"]; !reflect.DeepEqual(got, map[string]any{"inMailbox": "inbox-1"}) {
				t.Errorf("recovery query filter = %#v", got)
			}
			writeJMAPTestResponse(t, w, method, map[string]any{
				"queryState":          "fresh-state",
				"canCalculateChanges": true,
				"position":            0,
				"total":               1,
				"ids":                 []any{"email-1"},
			})
		case "Email/get":
			if got := args["ids"]; !reflect.DeepEqual(got, []any{"email-1"}) {
				t.Errorf("recovery Email/get ids = %#v, want email-1", got)
			}
			writeJMAPTestResponse(t, w, method, map[string]any{
				"list": []any{map[string]any{"id": "email-1"}},
			})
		default:
			t.Errorf("unexpected JMAP method %q", method)
			writeJMAPTestResponse(t, w, "error", map[string]any{"type": "unknownMethod"})
		}
	})

	emails, state, err := client.FetchChanges(context.Background(), "expired-state")
	if err != nil {
		t.Fatalf("fetch changes: %v", err)
	}
	if state != "fresh-state" {
		t.Fatalf("state = %q, want fresh-state", state)
	}
	if len(emails) != 1 || emails[0].ID != "email-1" {
		t.Fatalf("recovered emails = %#v, want email-1", emails)
	}
	if want := []string{"Email/queryChanges", "Email/query", "Email/get"}; !reflect.DeepEqual(methods, want) {
		t.Fatalf("method order = %#v, want %#v", methods, want)
	}
}

func TestFetchChangesRecoveryPaginatesAllEmailsAndBatchesGet(t *testing.T) {
	allIDs := makeTestEmailIDs(121)
	var queryPositions []int
	var getBatchSizes []int
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, args map[string]any) {
		switch method {
		case "Email/queryChanges":
			writeJMAPTestResponse(t, w, "error", map[string]any{"type": "tooManyChanges"})
		case "Email/query":
			if calculateTotal, _ := args["calculateTotal"].(bool); !calculateTotal {
				t.Error("recovery query must request calculateTotal")
			}
			position := int(args["position"].(float64))
			queryPositions = append(queryPositions, position)
			end := position + recoveryQueryPageSize
			if end > len(allIDs) {
				end = len(allIDs)
			}
			writeJMAPTestResponse(t, w, method, recoveryQueryResponse("recovered-state", position, len(allIDs), allIDs[position:end]))
		case "Email/get":
			ids := toStringSlice(args["ids"])
			if len(ids) > 25 {
				t.Errorf("Email/get batch size = %d, max = 25", len(ids))
			}
			getBatchSizes = append(getBatchSizes, len(ids))
			writeJMAPTestResponse(t, w, method, map[string]any{"list": emailListForIDs(ids)})
		default:
			t.Errorf("unexpected JMAP method %q", method)
			writeJMAPTestResponse(t, w, "error", map[string]any{"type": "unknownMethod"})
		}
	})
	client.maxObjectsInGet = 25

	emails, state, err := client.FetchChanges(context.Background(), "expired-state")
	if err != nil {
		t.Fatalf("fetch changes: %v", err)
	}
	if state != "recovered-state" {
		t.Fatalf("state = %q, want recovered-state", state)
	}
	if len(emails) != len(allIDs) {
		t.Fatalf("emails = %d, want %d", len(emails), len(allIDs))
	}
	for index, email := range emails {
		if email.ID != allIDs[index] {
			t.Fatalf("emails[%d].ID = %q, want %q", index, email.ID, allIDs[index])
		}
	}
	if want := []int{0, 50, 100}; !reflect.DeepEqual(queryPositions, want) {
		t.Fatalf("query positions = %#v, want %#v", queryPositions, want)
	}
	if want := []int{25, 25, 25, 25, 21}; !reflect.DeepEqual(getBatchSizes, want) {
		t.Fatalf("Email/get batch sizes = %#v, want %#v", getBatchSizes, want)
	}
}

func TestFetchChangesReturnsNoEmailsAndPreservesCursorWhenLaterBatchFails(t *testing.T) {
	getCalls := 0
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, args map[string]any) {
		switch method {
		case "Email/queryChanges":
			writeJMAPTestResponse(t, w, method, map[string]any{
				"oldQueryState": "query-state-1",
				"newQueryState": "query-state-2",
				"added": []any{
					map[string]any{"id": "email-1", "index": 0},
					map[string]any{"id": "email-2", "index": 1},
					map[string]any{"id": "email-3", "index": 2},
				},
			})
		case "Email/get":
			getCalls++
			ids := toStringSlice(args["ids"])
			if getCalls == 1 {
				if want := []string{"email-1", "email-2"}; !reflect.DeepEqual(ids, want) {
					t.Errorf("first Email/get ids = %#v, want %#v", ids, want)
				}
				writeJMAPTestResponse(t, w, method, map[string]any{"list": emailListForIDs(ids)})
				return
			}
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected JMAP method %q", method)
			writeJMAPTestResponse(t, w, "error", map[string]any{"type": "unknownMethod"})
		}
	})
	client.maxObjectsInGet = 2

	emails, state, err := client.FetchChanges(context.Background(), "query-state-1")
	if err == nil {
		t.Fatal("expected second Email/get batch to fail")
	}
	if len(emails) != 0 {
		t.Fatalf("emails = %#v, want no partial batch results", emails)
	}
	if state != "query-state-1" {
		t.Fatalf("state = %q, want original query-state-1", state)
	}
	if getCalls != 2 {
		t.Fatalf("Email/get calls = %d, want 2", getCalls)
	}
}

func TestFetchChangesRejectsCanCalculateChangesFalse(t *testing.T) {
	t.Run("initial query", func(t *testing.T) {
		client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
			if method != "Email/query" {
				t.Errorf("method = %q, want Email/query", method)
			}
			writeJMAPTestResponse(t, w, method, map[string]any{
				"queryState":          "unusable-state",
				"canCalculateChanges": false,
				"ids":                 []any{},
			})
		})

		emails, state, err := client.FetchChanges(context.Background(), "")
		if err == nil {
			t.Fatal("expected canCalculateChanges=false error")
		}
		if len(emails) != 0 || state != "" {
			t.Fatalf("emails/state = %#v/%q, want none and empty cursor", emails, state)
		}
	})

	t.Run("recovery query", func(t *testing.T) {
		client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
			switch method {
			case "Email/queryChanges":
				writeJMAPTestResponse(t, w, "error", map[string]any{"type": "cannotCalculateChanges"})
			case "Email/query":
				writeJMAPTestResponse(t, w, method, map[string]any{
					"queryState":          "unusable-state",
					"canCalculateChanges": false,
					"position":            0,
					"total":               0,
					"ids":                 []any{},
				})
			default:
				t.Errorf("unexpected JMAP method %q", method)
				writeJMAPTestResponse(t, w, "error", map[string]any{"type": "unknownMethod"})
			}
		})

		emails, state, err := client.FetchChanges(context.Background(), "expired-state")
		if err == nil {
			t.Fatal("expected canCalculateChanges=false recovery error")
		}
		if len(emails) != 0 || state != "expired-state" {
			t.Fatalf("emails/state = %#v/%q, want none and original cursor", emails, state)
		}
	})
}

func TestFetchChangesRecoveryRetriesStateDrift(t *testing.T) {
	allIDs := makeTestEmailIDs(51)
	attempt := 0
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, args map[string]any) {
		switch method {
		case "Email/queryChanges":
			writeJMAPTestResponse(t, w, "error", map[string]any{"type": "cannotCalculateChanges"})
		case "Email/query":
			position := int(args["position"].(float64))
			if position == 0 {
				attempt++
			}
			if attempt == 1 && position == 50 {
				writeJMAPTestResponse(t, w, method, recoveryQueryResponse("drifted-state", 40, 40, nil))
				return
			}
			state := fmt.Sprintf("stable-state-%d", attempt)
			end := position + recoveryQueryPageSize
			if end > len(allIDs) {
				end = len(allIDs)
			}
			writeJMAPTestResponse(t, w, method, recoveryQueryResponse(state, position, len(allIDs), allIDs[position:end]))
		case "Email/get":
			ids := toStringSlice(args["ids"])
			writeJMAPTestResponse(t, w, method, map[string]any{"list": emailListForIDs(ids)})
		default:
			t.Errorf("unexpected JMAP method %q", method)
			writeJMAPTestResponse(t, w, "error", map[string]any{"type": "unknownMethod"})
		}
	})

	emails, state, err := client.FetchChanges(context.Background(), "expired-state")
	if err != nil {
		t.Fatalf("fetch changes: %v", err)
	}
	if attempt != 2 {
		t.Fatalf("recovery attempts = %d, want 2", attempt)
	}
	if state != "stable-state-2" || len(emails) != len(allIDs) {
		t.Fatalf("state/emails = %q/%d, want stable-state-2/%d", state, len(emails), len(allIDs))
	}
}

func TestFetchChangesRecoveryRejectsPersistentStateDrift(t *testing.T) {
	attempt := 0
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, args map[string]any) {
		switch method {
		case "Email/queryChanges":
			writeJMAPTestResponse(t, w, "error", map[string]any{"type": "cannotCalculateChanges"})
		case "Email/query":
			position := int(args["position"].(float64))
			if position == 0 {
				attempt++
			}
			state := fmt.Sprintf("state-%d", attempt)
			if position == 50 {
				state = fmt.Sprintf("drift-%d", attempt)
			}
			ids := makeTestEmailIDs(51)[position:]
			if len(ids) > recoveryQueryPageSize {
				ids = ids[:recoveryQueryPageSize]
			}
			writeJMAPTestResponse(t, w, method, recoveryQueryResponse(state, position, 51, ids))
		default:
			t.Errorf("unexpected JMAP method %q", method)
			writeJMAPTestResponse(t, w, "error", map[string]any{"type": "unknownMethod"})
		}
	})

	emails, state, err := client.FetchChanges(context.Background(), "expired-state")
	if err == nil {
		t.Fatal("expected persistent recovery state drift error")
	}
	if attempt != recoveryQueryMaxRetries {
		t.Fatalf("recovery attempts = %d, want %d", attempt, recoveryQueryMaxRetries)
	}
	if len(emails) != 0 || state != "expired-state" {
		t.Fatalf("emails/state = %#v/%q, want none and original cursor", emails, state)
	}
}

func TestRecoveryQueryRejectsInvalidPages(t *testing.T) {
	tests := []struct {
		name     string
		response map[string]any
	}{
		{
			name:     "premature empty page",
			response: recoveryQueryResponse("state", 0, 1, nil),
		},
		{
			name:     "duplicate ids",
			response: recoveryQueryResponse("state", 0, 2, []string{"email-1", "email-1"}),
		},
		{
			name:     "wrong position",
			response: recoveryQueryResponse("state", 1, 1, []string{"email-1"}),
		},
		{
			name:     "page exceeds total",
			response: recoveryQueryResponse("state", 0, 1, []string{"email-1", "email-2"}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
				if method != "Email/query" {
					t.Errorf("method = %q, want Email/query", method)
				}
				writeJMAPTestResponse(t, w, method, tt.response)
			})
			if _, _, err := client.emailQueryAll(context.Background()); err == nil {
				t.Fatal("expected invalid recovery page error")
			}
		})
	}
}

func TestCallReturnsTypedJMAPMethodError(t *testing.T) {
	client := newJMAPTestClient(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
		writeJMAPTestResponse(t, w, "error", map[string]any{
			"type":        "cannotCalculateChanges",
			"description": "checkpoint expired",
		})
	})

	_, err := client.call(context.Background(), "Email/queryChanges", map[string]any{})
	var methodErr *JMAPMethodError
	if !errors.As(err, &methodErr) {
		t.Fatalf("error = %T %v, want *JMAPMethodError", err, err)
	}
	if methodErr.Method != "Email/queryChanges" || methodErr.Type != "cannotCalculateChanges" || methodErr.Description != "checkpoint expired" {
		t.Fatalf("method error = %#v", methodErr)
	}
}

func TestEnsureSessionParsesMaxObjectsInGet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"apiUrl": "/api",
			"capabilities": map[string]any{
				coreCapability: map[string]any{"maxObjectsInGet": 17},
			},
			"primaryAccounts": map[string]any{mailCapability: "account-1"},
		}); err != nil {
			t.Errorf("encode session: %v", err)
		}
	}))
	defer server.Close()

	client := newJMAPDiscoveryClient(t, server)
	if err := client.ensureSession(context.Background()); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if client.maxObjectsInGet != 17 {
		t.Fatalf("maxObjectsInGet = %d, want 17", client.maxObjectsInGet)
	}
	if client.apiURL != server.URL+"/api" || client.accountID != "account-1" {
		t.Fatalf("apiURL/accountID = %q/%q", client.apiURL, client.accountID)
	}
}

func TestEnsureSessionHonorsConfiguredAccountID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"apiUrl": "/api",
			"capabilities": map[string]any{
				coreCapability: map[string]any{"maxObjectsInGet": 17},
			},
			"primaryAccounts": map[string]any{mailCapability: "primary-account"},
			"accounts": map[string]any{
				"primary-account": map[string]any{
					"accountCapabilities": map[string]any{mailCapability: map[string]any{}},
				},
				"configured-account": map[string]any{
					"accountCapabilities": map[string]any{mailCapability: map[string]any{}},
				},
			},
		}); err != nil {
			t.Errorf("encode session: %v", err)
		}
	}))
	defer server.Close()

	client := newJMAPDiscoveryClient(t, server)
	client.cfg.JMAP.AccountID = "configured-account"
	if err := client.ensureSession(context.Background()); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if client.accountID != "configured-account" {
		t.Fatalf("accountID = %q, want configured-account", client.accountID)
	}
}

func TestEnsureSessionRejectsInvalidConfiguredAccountID(t *testing.T) {
	tests := []struct {
		name      string
		accountID string
		wantError string
	}{
		{
			name:      "unknown account",
			accountID: "missing-account",
			wantError: `configured JMAP account "missing-account" not found in session`,
		},
		{
			name:      "account without mail capability",
			accountID: "non-mail-account",
			wantError: `configured JMAP account "non-mail-account" does not support mail`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{
					"apiUrl": "/api",
					"capabilities": map[string]any{
						coreCapability: map[string]any{"maxObjectsInGet": 17},
					},
					"primaryAccounts": map[string]any{mailCapability: "primary-account"},
					"accounts": map[string]any{
						"primary-account": map[string]any{
							"accountCapabilities": map[string]any{mailCapability: map[string]any{}},
						},
						"non-mail-account": map[string]any{
							"accountCapabilities": map[string]any{coreCapability: map[string]any{}},
						},
					},
				}); err != nil {
					t.Errorf("encode session: %v", err)
				}
			}))
			defer server.Close()

			client := newJMAPDiscoveryClient(t, server)
			client.cfg.JMAP.AccountID = tt.accountID
			err := client.ensureSession(context.Background())
			if err == nil || err.Error() != tt.wantError {
				t.Fatalf("ensure session error = %v, want %q", err, tt.wantError)
			}
			if client.apiURL != "" || client.accountID != "" || client.maxObjectsInGet != 0 {
				t.Fatalf("invalid configured account partially mutated client: %#v", client)
			}
		})
	}
}

func TestEnsureSessionRejectsInvalidMaxObjectsInGet(t *testing.T) {
	tests := []struct {
		name             string
		coreCapabilities map[string]any
	}{
		{name: "missing core capability"},
		{name: "missing max", coreCapabilities: map[string]any{}},
		{name: "zero max", coreCapabilities: map[string]any{"maxObjectsInGet": 0}},
		{name: "negative max", coreCapabilities: map[string]any{"maxObjectsInGet": -1}},
		{name: "non-integer max", coreCapabilities: map[string]any{"maxObjectsInGet": "many"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				capabilities := map[string]any{}
				if tt.coreCapabilities != nil {
					capabilities[coreCapability] = tt.coreCapabilities
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{
					"apiUrl":          "/api",
					"capabilities":    capabilities,
					"primaryAccounts": map[string]any{mailCapability: "account-1"},
				}); err != nil {
					t.Errorf("encode session: %v", err)
				}
			}))
			defer server.Close()

			client := newJMAPDiscoveryClient(t, server)
			if err := client.ensureSession(context.Background()); err == nil {
				t.Fatal("expected invalid maxObjectsInGet error")
			}
			if client.apiURL != "" || client.accountID != "" || client.maxObjectsInGet != 0 {
				t.Fatalf("invalid session partially mutated client: %#v", client)
			}
		})
	}
}

func TestCallUsesCoreAndMailCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Using []string `json:"using"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if want := []string{coreCapability, mailCapability}; !reflect.DeepEqual(request.Using, want) {
			t.Errorf("using = %#v, want %#v", request.Using, want)
		}
		writeJMAPTestResponse(t, w, "Mailbox/get", map[string]any{"list": []any{}})
	}))
	defer server.Close()

	var cfg config.Config
	cfg.JMAP.Username = "test-user"
	cfg.JMAP.Password = "test-password"
	client := &JMAPClient{cfg: cfg, httpClient: server.Client(), apiURL: server.URL}
	if _, err := client.call(context.Background(), "Mailbox/get", map[string]any{}); err != nil {
		t.Fatalf("call: %v", err)
	}
}

func TestFetchChangesRejectsMalformedQueryChanges(t *testing.T) {
	tests := []struct {
		name     string
		response map[string]any
	}{
		{
			name: "mismatched old state",
			response: map[string]any{
				"oldQueryState": "wrong-state",
				"newQueryState": "query-state-2",
				"added":         []any{},
			},
		},
		{
			name: "malformed added",
			response: map[string]any{
				"oldQueryState": "query-state-1",
				"newQueryState": "query-state-2",
				"added":         []any{"email-1"},
			},
		},
		{
			name: "missing added",
			response: map[string]any{
				"oldQueryState": "query-state-1",
				"newQueryState": "query-state-2",
			},
		},
		{
			name: "missing added index",
			response: map[string]any{
				"oldQueryState": "query-state-1",
				"newQueryState": "query-state-2",
				"added":         []any{map[string]any{"id": "email-1"}},
			},
		},
		{
			name: "fractional added index",
			response: map[string]any{
				"oldQueryState": "query-state-1",
				"newQueryState": "query-state-2",
				"added":         []any{map[string]any{"id": "email-1", "index": 0.5}},
			},
		},
		{
			name: "duplicate added index",
			response: map[string]any{
				"oldQueryState": "query-state-1",
				"newQueryState": "query-state-2",
				"added": []any{
					map[string]any{"id": "email-1", "index": 1},
					map[string]any{"id": "email-2", "index": 1},
				},
			},
		},
		{
			name: "out-of-order added index",
			response: map[string]any{
				"oldQueryState": "query-state-1",
				"newQueryState": "query-state-2",
				"added": []any{
					map[string]any{"id": "email-1", "index": 2},
					map[string]any{"id": "email-2", "index": 1},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
				if method != "Email/queryChanges" {
					t.Errorf("method = %q, want Email/queryChanges", method)
				}
				writeJMAPTestResponse(t, w, method, tt.response)
			})
			emails, state, err := client.FetchChanges(context.Background(), "query-state-1")
			if err == nil {
				t.Fatal("expected malformed queryChanges error")
			}
			if len(emails) != 0 || state != "query-state-1" {
				t.Fatalf("emails/state = %#v/%q, want none and original cursor", emails, state)
			}
		})
	}
}

func TestFetchChangesRejectsMissingQueryStates(t *testing.T) {
	tests := []struct {
		name     string
		since    string
		method   string
		response map[string]any
	}{
		{
			name:     "initial queryState",
			method:   "Email/query",
			response: map[string]any{"canCalculateChanges": true, "ids": []any{"email-1"}},
		},
		{
			name:     "incremental newQueryState",
			since:    "query-state-1",
			method:   "Email/queryChanges",
			response: map[string]any{"added": []any{map[string]any{"id": "email-1"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
				calls++
				if method != tt.method {
					t.Errorf("method = %q, want %q", method, tt.method)
					writeJMAPTestResponse(t, w, "error", map[string]any{"type": "unknownMethod"})
					return
				}
				writeJMAPTestResponse(t, w, method, tt.response)
			})

			emails, state, err := client.FetchChanges(context.Background(), tt.since)
			if err == nil {
				t.Fatal("expected missing state error")
			}
			if state != tt.since {
				t.Fatalf("state = %q, want caller cursor %q", state, tt.since)
			}
			if len(emails) != 0 {
				t.Fatalf("emails = %#v, want none", emails)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want 1", calls)
			}
		})
	}
}

func TestFetchChangesPreservesCursorWhenRecoveryFails(t *testing.T) {
	tests := []struct {
		name       string
		queryReply map[string]any
		failGet    bool
	}{
		{
			name:       "malformed recovery query",
			queryReply: map[string]any{"ids": []any{"email-1"}},
		},
		{
			name: "recovery Email/get failure",
			queryReply: map[string]any{
				"queryState":          "fresh-state",
				"canCalculateChanges": true,
				"position":            0,
				"total":               1,
				"ids":                 []any{"email-1"},
			},
			failGet: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
				switch method {
				case "Email/queryChanges":
					writeJMAPTestResponse(t, w, "error", map[string]any{"type": "cannotCalculateChanges"})
				case "Email/query":
					writeJMAPTestResponse(t, w, method, tt.queryReply)
				case "Email/get":
					if !tt.failGet {
						t.Errorf("unexpected Email/get")
					}
					http.Error(w, "temporary failure", http.StatusServiceUnavailable)
				default:
					t.Errorf("unexpected JMAP method %q", method)
					writeJMAPTestResponse(t, w, "error", map[string]any{"type": "unknownMethod"})
				}
			})

			emails, state, err := client.FetchChanges(context.Background(), "expired-state")
			if err == nil {
				t.Fatal("expected recovery failure")
			}
			if state != "expired-state" {
				t.Fatalf("state = %q, want caller cursor expired-state", state)
			}
			if len(emails) != 0 {
				t.Fatalf("emails = %#v, want none", emails)
			}
		})
	}
}

func TestFetchChangesScopesInitialAndIncrementalQueriesToInbox(t *testing.T) {
	expectedFilter := map[string]any{"inMailbox": "inbox-1"}
	expectedSort := []any{map[string]any{
		"property":    "receivedAt",
		"isAscending": false,
	}}

	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, args map[string]any) {
		if !reflect.DeepEqual(args["filter"], expectedFilter) {
			t.Errorf("%s filter = %#v, want %#v", method, args["filter"], expectedFilter)
		}
		if !reflect.DeepEqual(args["sort"], expectedSort) {
			t.Errorf("%s sort = %#v, want %#v", method, args["sort"], expectedSort)
		}

		switch method {
		case "Email/query":
			writeJMAPTestResponse(t, w, method, map[string]any{
				"queryState":          "query-state-1",
				"canCalculateChanges": true,
				"ids":                 []any{},
			})
		case "Email/queryChanges":
			writeJMAPTestResponse(t, w, method, map[string]any{
				"oldQueryState": "query-state-1",
				"newQueryState": "query-state-2",
				"added":         []any{},
			})
		default:
			t.Errorf("unexpected JMAP method %q", method)
			writeJMAPTestResponse(t, w, "error", map[string]any{"type": "unknownMethod"})
		}
	})

	_, initialState, err := client.FetchChanges(context.Background(), "")
	if err != nil {
		t.Fatalf("initial query: %v", err)
	}
	if initialState != "query-state-1" {
		t.Fatalf("expected initial query state, got %q", initialState)
	}

	_, incrementalState, err := client.FetchChanges(context.Background(), initialState)
	if err != nil {
		t.Fatalf("incremental query: %v", err)
	}
	if incrementalState != "query-state-2" {
		t.Fatalf("expected incremental query state, got %q", incrementalState)
	}
}

func TestEmailGetRequestsTextAndHTMLBodyValues(t *testing.T) {
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, args map[string]any) {
		if method != "Email/get" {
			t.Errorf("expected Email/get, got %q", method)
		}
		if fetch, _ := args["fetchTextBodyValues"].(bool); !fetch {
			t.Error("Email/get must request fetchTextBodyValues")
		}
		if fetch, _ := args["fetchHTMLBodyValues"].(bool); !fetch {
			t.Error("Email/get must request fetchHTMLBodyValues")
		}
		writeJMAPTestResponse(t, w, method, map[string]any{
			"list": []any{map[string]any{"id": "email-1"}},
		})
	})

	if _, err := client.emailGet(context.Background(), []string{"email-1"}); err != nil {
		t.Fatalf("email get: %v", err)
	}
}

func TestEmailGetPreservesCCRecipients(t *testing.T) {
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, args map[string]any) {
		properties, ok := args["properties"].([]any)
		if !ok {
			t.Errorf("Email/get properties = %#v, want an array", args["properties"])
		}
		requestedCC := false
		for _, property := range properties {
			if property == "cc" {
				requestedCC = true
				break
			}
		}
		if !requestedCC {
			t.Error("Email/get must request cc")
		}
		writeJMAPTestResponse(t, w, method, map[string]any{
			"list": []any{map[string]any{
				"id": "email-1",
				"cc": []any{
					map[string]any{"name": "Casey", "email": "casey@example.com"},
					map[string]any{"name": "Dee", "email": "dee@example.com"},
				},
			}},
		})
	})

	emails, err := client.emailGet(context.Background(), []string{"email-1"})
	if err != nil {
		t.Fatalf("email get: %v", err)
	}
	want := []store.Participant{
		{Name: "Casey", Email: "casey@example.com"},
		{Name: "Dee", Email: "dee@example.com"},
	}
	if len(emails) != 1 || !reflect.DeepEqual(emails[0].CC, want) {
		t.Fatalf("email CC = %#v, want %#v", emails, want)
	}
}

func TestMessageFromEmailPreservesCC(t *testing.T) {
	want := []store.Participant{{Name: "Casey", Email: "casey@example.com"}}
	msg := messageFromEmail(Email{CC: want})
	if !reflect.DeepEqual(msg.CC, want) {
		t.Fatalf("message CC = %#v, want %#v", msg.CC, want)
	}
}

func TestEmailGetValidatesResponseAccounting(t *testing.T) {
	tests := []struct {
		name     string
		response map[string]any
		wantIDs  []string
		wantErr  bool
	}{
		{
			name: "complete list restored to request order",
			response: map[string]any{"list": []any{
				map[string]any{"id": "email-2"},
				map[string]any{"id": "email-1"},
			}},
			wantIDs: []string{"email-1", "email-2"},
		},
		{
			name: "not found accounts for deleted id",
			response: map[string]any{
				"list":     []any{map[string]any{"id": "email-2"}},
				"notFound": []any{"email-1"},
			},
			wantIDs: []string{"email-2"},
		},
		{
			name:     "missing requested id",
			response: map[string]any{"list": []any{map[string]any{"id": "email-1"}}},
			wantErr:  true,
		},
		{
			name:     "malformed list item",
			response: map[string]any{"list": []any{"email-1", map[string]any{"id": "email-2"}}},
			wantErr:  true,
		},
		{
			name: "unknown list id",
			response: map[string]any{"list": []any{
				map[string]any{"id": "email-1"},
				map[string]any{"id": "email-3"},
			}},
			wantErr: true,
		},
		{
			name: "duplicate list id",
			response: map[string]any{"list": []any{
				map[string]any{"id": "email-1"},
				map[string]any{"id": "email-1"},
			}},
			wantErr: true,
		},
		{
			name: "list and notFound collision",
			response: map[string]any{
				"list": []any{
					map[string]any{"id": "email-1"},
					map[string]any{"id": "email-2"},
				},
				"notFound": []any{"email-1"},
			},
			wantErr: true,
		},
		{
			name: "unknown notFound id",
			response: map[string]any{
				"list":     []any{map[string]any{"id": "email-1"}},
				"notFound": []any{"email-3"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
				writeJMAPTestResponse(t, w, method, tt.response)
			})
			emails, err := client.emailGet(context.Background(), []string{"email-1", "email-2"})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected invalid Email/get response error")
				}
				return
			}
			if err != nil {
				t.Fatalf("email get: %v", err)
			}
			gotIDs := make([]string, len(emails))
			for index, email := range emails {
				gotIDs[index] = email.ID
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Fatalf("email ids = %#v, want %#v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestFetchChangesAdvancesCursorWhenNoEmailsAreAdded(t *testing.T) {
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
		if method != "Email/queryChanges" {
			t.Errorf("expected Email/queryChanges, got %q", method)
		}
		writeJMAPTestResponse(t, w, method, map[string]any{
			"oldQueryState": "query-state-1",
			"newQueryState": "query-state-2",
			"added":         []any{},
		})
	})

	emails, state, err := client.FetchChanges(context.Background(), "query-state-1")
	if err != nil {
		t.Fatalf("fetch changes: %v", err)
	}
	if len(emails) != 0 {
		t.Fatalf("expected no emails, got %d", len(emails))
	}
	if state != "query-state-2" {
		t.Fatalf("expected empty change set to advance cursor, got %q", state)
	}
}

func TestFetchChangesPreservesCursorWhenEmailGetFails(t *testing.T) {
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
		switch method {
		case "Email/queryChanges":
			writeJMAPTestResponse(t, w, method, map[string]any{
				"oldQueryState": "query-state-1",
				"newQueryState": "query-state-2",
				"added": []any{
					map[string]any{"id": "email-1", "index": 0},
				},
			})
		case "Email/get":
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected JMAP method %q", method)
			writeJMAPTestResponse(t, w, "error", map[string]any{"type": "unknownMethod"})
		}
	})

	emails, state, err := client.FetchChanges(context.Background(), "query-state-1")
	if err == nil {
		t.Fatal("expected Email/get failure")
	}
	if len(emails) != 0 {
		t.Fatalf("expected no emails on failure, got %d", len(emails))
	}
	if state != "query-state-1" {
		t.Fatalf("expected failed fetch to preserve cursor, got %q", state)
	}
}

func TestEmailGetUsesFirstMessageID(t *testing.T) {
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
		if method != "Email/get" {
			t.Errorf("expected Email/get, got %q", method)
		}
		writeJMAPTestResponse(t, w, method, map[string]any{
			"list": []any{map[string]any{
				"id":        "email-1",
				"messageId": []any{"first@example.test", "second@example.test"},
			}},
		})
	})

	emails, err := client.emailGet(context.Background(), []string{"email-1"})
	if err != nil {
		t.Fatalf("email get: %v", err)
	}
	if len(emails) != 1 {
		t.Fatalf("expected one email, got %d", len(emails))
	}
	if emails[0].InternetMsg != "first@example.test" {
		t.Fatalf("expected first RFC messageId, got %q", emails[0].InternetMsg)
	}
}

func TestEmailGetConcatenatesMultipartBodiesInOrder(t *testing.T) {
	client := newJMAPTestClient(t, func(w http.ResponseWriter, method string, _ map[string]any) {
		if method != "Email/get" {
			t.Errorf("expected Email/get, got %q", method)
		}
		writeJMAPTestResponse(t, w, method, map[string]any{
			"list": []any{map[string]any{
				"id": "email-1",
				"bodyValues": map[string]any{
					"text-1": map[string]any{"value": "first"},
					"text-2": map[string]any{"value": "second"},
					"html-1": map[string]any{"value": "<p>one</p>"},
					"html-2": map[string]any{"value": "<p>two</p>"},
				},
				"textBody": []any{
					map[string]any{"partId": "text-2"},
					map[string]any{"partId": "text-1"},
				},
				"htmlBody": []any{
					map[string]any{"partId": "html-1"},
					map[string]any{"partId": "html-2"},
				},
			}},
		})
	})

	emails, err := client.emailGet(context.Background(), []string{"email-1"})
	if err != nil {
		t.Fatalf("email get: %v", err)
	}
	if len(emails) != 1 {
		t.Fatalf("expected one email, got %d", len(emails))
	}
	if emails[0].Text != "secondfirst" {
		t.Fatalf("expected ordered text parts, got %q", emails[0].Text)
	}
	if emails[0].HTML != "<p>one</p><p>two</p>" {
		t.Fatalf("expected ordered HTML parts, got %q", emails[0].HTML)
	}
}

func newJMAPTestClient(t *testing.T, handler func(http.ResponseWriter, string, map[string]any)) *JMAPClient {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, args, err := readJMAPTestCall(r)
		if err != nil {
			t.Errorf("decode JMAP request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		handler(w, method, args)
	}))
	t.Cleanup(server.Close)

	var cfg config.Config
	cfg.JMAP.URL = server.URL
	cfg.JMAP.Username = "test-user"
	cfg.JMAP.Password = "test-password"
	return &JMAPClient{
		cfg:             cfg,
		httpClient:      server.Client(),
		apiURL:          server.URL,
		accountID:       "account-1",
		inboxMailboxID:  "inbox-1",
		maxObjectsInGet: initialEmailQueryLimit,
	}
}

func newJMAPDiscoveryClient(t *testing.T, server *httptest.Server) *JMAPClient {
	t.Helper()
	var cfg config.Config
	cfg.JMAP.URL = server.URL
	cfg.JMAP.SessionURL = server.URL
	cfg.JMAP.Username = "test-user"
	cfg.JMAP.Password = "test-password"
	client, err := NewJMAPClient(cfg)
	if err != nil {
		t.Fatalf("new JMAP client: %v", err)
	}
	client.httpClient = server.Client()
	return client
}

func makeTestEmailIDs(count int) []string {
	ids := make([]string, count)
	for index := range ids {
		ids[index] = fmt.Sprintf("email-%03d", index+1)
	}
	return ids
}

func recoveryQueryResponse(state string, position int, total int, ids []string) map[string]any {
	rawIDs := make([]any, len(ids))
	for index, id := range ids {
		rawIDs[index] = id
	}
	return map[string]any{
		"queryState":          state,
		"canCalculateChanges": true,
		"position":            position,
		"total":               total,
		"ids":                 rawIDs,
	}
}

func emailListForIDs(ids []string) []any {
	list := make([]any, len(ids))
	for index, id := range ids {
		list[index] = map[string]any{"id": id}
	}
	return list
}

func readJMAPTestCall(r *http.Request) (string, map[string]any, error) {
	var request struct {
		MethodCalls []json.RawMessage `json:"methodCalls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return "", nil, err
	}
	if len(request.MethodCalls) != 1 {
		return "", nil, fmt.Errorf("expected one method call, got %d", len(request.MethodCalls))
	}

	var call []json.RawMessage
	if err := json.Unmarshal(request.MethodCalls[0], &call); err != nil {
		return "", nil, err
	}
	if len(call) < 2 {
		return "", nil, fmt.Errorf("invalid method call length %d", len(call))
	}

	var method string
	if err := json.Unmarshal(call[0], &method); err != nil {
		return "", nil, err
	}
	var args map[string]any
	if err := json.Unmarshal(call[1], &args); err != nil {
		return "", nil, err
	}
	return method, args, nil
}

func writeJMAPTestResponse(t *testing.T, w http.ResponseWriter, method string, args map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"methodResponses": []any{
			[]any{method, args, "c1"},
		},
	}); err != nil {
		t.Errorf("encode JMAP response: %v", err)
	}
}
