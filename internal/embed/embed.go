package embed

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"time"
)

type Provider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
	Name() string
}

type Noop struct {
	dim int
}

func NewNoop(dim int) *Noop {
	if dim <= 0 {
		dim = 1536
	}
	return &Noop{dim: dim}
}

func (n *Noop) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		out = append(out, pseudoVector(text, n.dim))
	}
	return out, nil
}

func (n *Noop) Dim() int {
	return n.dim
}

func (n *Noop) Name() string {
	return "noop"
}

func pseudoVector(text string, dim int) []float32 {
	h := sha256.Sum256([]byte(text))
	seed := int64(binary.LittleEndian.Uint64(h[:8]))
	rnd := rand.New(rand.NewSource(seed))
	vec := make([]float32, dim)
	for i := 0; i < dim; i++ {
		vec[i] = float32(rnd.Float64()*2 - 1)
	}
	return normalize(vec)
}

func normalize(vec []float32) []float32 {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return vec
	}
	inv := 1 / float32(math.Sqrt(sum))
	for i := range vec {
		vec[i] *= inv
	}
	return vec
}

type Disabled struct{}

func (d Disabled) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, errors.New("embedding provider disabled")
}

func (d Disabled) Dim() int {
	return 0
}

func (d Disabled) Name() string {
	return "disabled"
}

func Sleepy(ctx context.Context, dur time.Duration) error {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
