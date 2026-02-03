package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type OpenAI struct {
	APIKey string
	Model  string
	DimVal int
	Client *http.Client
}

func NewOpenAI(apiKey string, model string, dim int) *OpenAI {
	if model == "" {
		model = "text-embedding-3-small"
	}
	if dim <= 0 {
		dim = 1536
	}
	return &OpenAI{
		APIKey: apiKey,
		Model:  model,
		DimVal: dim,
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (o *OpenAI) Name() string {
	return "openai"
}

func (o *OpenAI) Dim() int {
	return o.DimVal
}

func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if o.APIKey == "" {
		return nil, errors.New("openai api key not configured")
	}
	payload := map[string]any{
		"model": o.Model,
		"input": texts,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("openai embedding request failed")
	}

	var decoded struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	out := make([][]float32, 0, len(decoded.Data))
	for _, item := range decoded.Data {
		out = append(out, item.Embedding)
	}
	return out, nil
}
