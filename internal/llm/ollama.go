package llm

import (
	"context"
	"errors"
)

type Ollama struct {
	BaseURL string
	ModelName   string
}

func NewOllama(baseURL string, model string) *Ollama {
	if model == "" {
		model = "llama3"
	}
	return &Ollama{BaseURL: baseURL, ModelName: model}
}

func (o *Ollama) Name() string { return "ollama" }
func (o *Ollama) Model() string { return o.ModelName }

func (o *Ollama) Classify(_ context.Context, _ string, _ map[string]any) (Classification, error) {
	return Classification{}, errors.New("ollama provider not implemented")
}

func (o *Ollama) Extract(_ context.Context, _ string, _ map[string]any, _ []map[string]any) (Extraction, error) {
	return Extraction{}, errors.New("ollama provider not implemented")
}

func (o *Ollama) Draft(_ context.Context, _ string, _ map[string]any, _ string) (Draft, error) {
	return Draft{}, errors.New("ollama provider not implemented")
}
