package store

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

func TestOpenDBUsesTransactionPoolerSafeQueryMode(t *testing.T) {
	for _, dsn := range []string{
		"postgres://user:password@db.example.test:5432/database?sslmode=disable",
		"postgres://user:password@pooler.example.test:6543/database?sslmode=disable&default_query_exec_mode=cache_statement",
	} {
		config, err := poolerSafePGXConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if config.DefaultQueryExecMode != pgx.QueryExecModeExec {
			t.Fatalf("query mode = %s, want exec", config.DefaultQueryExecMode)
		}
		if config.StatementCacheCapacity != 0 || config.DescriptionCacheCapacity != 0 {
			t.Fatalf("cache capacities = (%d, %d), want zero", config.StatementCacheCapacity, config.DescriptionCacheCapacity)
		}
	}
}

func TestOpenDBExecutesParameterizedQueryBeforePoolerCanReassign(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverResult := make(chan error, 1)
	go func() {
		serverResult <- servePoolerBoundary(listener)
	}()

	dsn := fmt.Sprintf("postgres://user@%s/database?sslmode=disable", listener.Addr())
	db, err := OpenDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got string
	if err := db.QueryRowContext(ctx, `SELECT $1::text`, "pooler-safe").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "pooler-safe" {
		t.Fatalf("result = %q, want pooler-safe", got)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

// servePoolerBoundary models the point at which a transaction pooler may
// return a backend after Sync. A parameterized query is safe only when Execute
// reaches that boundary with Parse and Bind; describe-exec fails this contract
// because its first Sync arrives before Bind/Execute.
func servePoolerBoundary(listener net.Listener) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	backend := pgproto3.NewBackend(conn, conn)
	if _, err := backend.ReceiveStartupMessage(); err != nil {
		return err
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "16.0"})
	backend.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return err
	}

	sawParse := false
	sawBind := false
	sawExecute := false
	for {
		message, err := backend.Receive()
		if err != nil {
			return err
		}
		switch message := message.(type) {
		case *pgproto3.Parse:
			if message.Name != "" {
				return fmt.Errorf("named prepared statement %q reached pooler boundary", message.Name)
			}
			sawParse = true
		case *pgproto3.Bind:
			sawBind = true
		case *pgproto3.Execute:
			sawExecute = true
		case *pgproto3.Sync:
			if !sawParse || !sawBind || !sawExecute {
				return fmt.Errorf("pooler boundary before complete query: parse=%t bind=%t execute=%t", sawParse, sawBind, sawExecute)
			}
			backend.Send(&pgproto3.ParseComplete{})
			backend.Send(&pgproto3.BindComplete{})
			backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{
				Name: []byte("text"), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1,
			}}})
			backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("pooler-safe")}})
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			return backend.Flush()
		}
	}
}

func TestOpenDBRejectsMissingDSN(t *testing.T) {
	if _, err := OpenDB(""); err == nil {
		t.Fatal("expected missing DSN error")
	}
}

func TestJSONTextArgPreservesNullAndEncodesJSONAsText(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want any
	}{
		{name: "sql null", raw: nil, want: nil},
		{name: "empty JSON object", raw: []byte(`{}`), want: `{}`},
		{name: "nonempty JSON", raw: []byte(`{"event":"sent"}`), want: `{"event":"sent"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := jsonTextArg(test.raw)
			if got != test.want {
				t.Fatalf("jsonTextArg(%q) = %#v, want %#v", test.raw, got, test.want)
			}
		})
	}
}
