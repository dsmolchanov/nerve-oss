package mcp

import (
	"context"
	"io"
	"strings"
	"testing"

	"neuralmail/internal/memguard"
)

type bytewiseBody struct{ io.Reader }

func (r *bytewiseBody) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}
func (*bytewiseBody) Close() error { return nil }

func TestBytewiseBodyBudgetBookkeepingDoesNotGrowPerRead(t *testing.T) {
	for _, copies := range []int64{1, 2} {
		payload := strings.Repeat("x", 64<<10)
		budget, err := memguard.New(2 * int64(len(payload)))
		if err != nil {
			t.Fatal(err)
		}
		allocations := testing.AllocsPerRun(2, func() {
			body := &bytewiseBody{Reader: strings.NewReader(payload)}
			guarded, err := newBudgetedReadCloserCopies(context.Background(), body, budget, 0, copies)
			if err != nil {
				panic(err)
			}
			if n, err := io.Copy(io.Discard, guarded); err != nil || n != int64(len(payload)) {
				panic("bytewise read failed")
			}
			if budget.Used() != copies*int64(len(payload)) {
				panic("bytewise body was not fully charged")
			}
			if err := guarded.Close(); err != nil {
				panic(err)
			}
			if budget.Used() != 0 {
				panic("bytewise body leaked")
			}
		})
		// A release closure for each of 65,536 one-byte reads amplifies input
		// into >65,000 heap allocations. Bookkeeping must stay bounded even
		// when wire fragmentation is controlled by the peer.
		if allocations > 1000 {
			t.Fatalf("%d wire copies: bytewise input created %.0f allocations", copies, allocations)
		}
	}
}
