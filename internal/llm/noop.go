package llm

import "context"

type Noop struct{}

func NewNoop() *Noop          { return &Noop{} }
func (n *Noop) Name() string  { return "noop" }
func (n *Noop) Model() string { return "noop" }

func (n *Noop) Classify(context.Context, string, map[string]any) (Classification, error) {
	return Classification{}, ErrUnavailable
}
func (n *Noop) Extract(context.Context, string, map[string]any, []map[string]any) (Extraction, error) {
	return Extraction{}, ErrUnavailable
}
func (n *Noop) Draft(context.Context, string, map[string]any, string) (Draft, error) {
	return Draft{}, ErrUnavailable
}
