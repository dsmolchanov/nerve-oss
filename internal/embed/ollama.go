package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type Ollama struct {
	BaseURL string
	Model   string
	DimVal  int
	Client  *http.Client
}

func NewOllama(baseURL string, model string, dim int) *Ollama {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	if dim <= 0 {
		dim = 768
	}
	return &Ollama{BaseURL: baseURL, Model: model, DimVal: dim, Client: &http.Client{Timeout: 30 * time.Second}}
}

func (o *Ollama) Name() string {
	return "ollama"
}

func (o *Ollama) Dim() int {
	return o.DimVal
}

func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if o.BaseURL == "" {
		return nil, errors.New("ollama url not configured")
	}
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		payload := map[string]any{
			"model":  o.Model,
			"prompt": text,
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/embeddings", o.BaseURL), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := o.Client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.Body != nil {
			defer resp.Body.Close()
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, errors.New("ollama embedding request failed")
		}
		var decoded struct {
			Embedding []float32 `json:"embedding"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			return nil, err
		}
		out = append(out, decoded.Embedding)
	}
	return out, nil
}
