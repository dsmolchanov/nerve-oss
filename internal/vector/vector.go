package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type Store interface {
	Upsert(ctx context.Context, points []Point) error
	Search(ctx context.Context, vector []float32, limit int, filter map[string]any) ([]SearchHit, error)
	EnsureCollection(ctx context.Context, dim int) error
	Name() string
}

type Point struct {
	ID     string         `json:"id"`
	Vector []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

type SearchHit struct {
	ID      string         `json:"id"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
}

type Qdrant struct {
	BaseURL    string
	Collection string
	Client     *http.Client
}

func NewQdrant(baseURL, collection string) *Qdrant {
	if collection == "" {
		collection = "messages_v1536"
	}
	return &Qdrant{BaseURL: baseURL, Collection: collection, Client: &http.Client{Timeout: 15 * time.Second}}
}

func (q *Qdrant) Name() string { return "qdrant" }

func (q *Qdrant) EnsureCollection(ctx context.Context, dim int) error {
	if q.BaseURL == "" {
		return errors.New("qdrant url not configured")
	}
	body := map[string]any{
		"vectors": map[string]any{
			"size":     dim,
			"distance": "Cosine",
		},
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/collections/%s", q.BaseURL, q.Collection), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.Client.Do(req)
	if err != nil {
		return err
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return errors.New("qdrant collection create failed")
}

func (q *Qdrant) Upsert(ctx context.Context, points []Point) error {
	if q.BaseURL == "" {
		return errors.New("qdrant url not configured")
	}
	payload := map[string]any{
		"points": points,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/collections/%s/points?wait=true", q.BaseURL, q.Collection), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.Client.Do(req)
	if err != nil {
		return err
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return errors.New("qdrant upsert failed")
}

func (q *Qdrant) Search(ctx context.Context, vector []float32, limit int, filter map[string]any) ([]SearchHit, error) {
	if q.BaseURL == "" {
		return nil, errors.New("qdrant url not configured")
	}
	if limit <= 0 {
		limit = 10
	}
	body := map[string]any{
		"vector": vector,
		"limit":  limit,
	}
	if filter != nil {
		body["filter"] = filter
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/collections/%s/points/search", q.BaseURL, q.Collection), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("qdrant search failed")
	}
	var decoded struct {
		Result []struct {
			ID      any             `json:"id"`
			Score   float64         `json:"score"`
			Payload map[string]any  `json:"payload"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(decoded.Result))
	for _, item := range decoded.Result {
		out = append(out, SearchHit{ID: fmt.Sprintf("%v", item.ID), Score: item.Score, Payload: item.Payload})
	}
	return out, nil
}
