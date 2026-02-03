package jmap

import (
	"context"
	"errors"
	"time"

	"neuralmail/internal/store"
)

type Email struct {
	ID          string
	ThreadID    string
	Subject     string
	Text        string
	HTML        string
	From        store.Participant
	To          []store.Participant
	ReceivedAt  time.Time
	InternetMsg string
}

type Client interface {
	FetchChanges(ctx context.Context, sinceState string) ([]Email, string, error)
	Name() string
}

type NoopClient struct{}

func (n NoopClient) FetchChanges(_ context.Context, _ string) ([]Email, string, error) {
	return nil, "", nil
}

func (n NoopClient) Name() string { return "noop" }

var ErrNotConfigured = errors.New("jmap client not configured")

func Ingest(ctx context.Context, client Client, store *store.Store, inboxID string, sinceState string) (string, []string, error) {
	emails, newState, err := client.FetchChanges(ctx, sinceState)
	if err != nil {
		return sinceState, nil, err
	}
	var ids []string
	for _, email := range emails {
		msg := store.Message{
			Direction:         "inbound",
			Subject:           email.Subject,
			Text:              email.Text,
			HTML:              email.HTML,
			CreatedAt:         email.ReceivedAt,
			ProviderMessageID: email.ID,
			InternetMessageID: email.InternetMsg,
			From:              email.From,
			To:                email.To,
		}
		_, msgID, err := store.InsertMessageWithThread(ctx, inboxID, msg)
		if err != nil {
			return sinceState, ids, err
		}
		ids = append(ids, msgID)
	}
	return newState, ids, nil
}
