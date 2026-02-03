package observability

import "github.com/google/uuid"

func NewReplayID() string {
	return uuid.NewString()
}
