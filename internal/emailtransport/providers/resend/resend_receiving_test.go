package resend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReceivingClientClassifiesRetentionNotFound(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer server.Close()
	client := NewReceivingClient(Config{BaseURL: server.URL, HTTPClient: server.Client()})

	if _, err := client.GetReceivedEmail(context.Background(), "received-1"); !errors.Is(err, ErrReceivedEmailNotFound) {
		t.Fatalf("received email error=%v, want typed not-found", err)
	}
	if _, err := client.GetAttachment(context.Background(), "received-1", "attachment-1"); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("attachment error=%v, want typed not-found", err)
	}
}

func TestReceivingClientReturnsAttachmentDownload(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"attachment-1","download_url":"https://download.example.com/file","filename":"report.pdf","content_type":"application/pdf"}`))
	}))
	defer server.Close()
	client := NewReceivingClient(Config{BaseURL: server.URL, HTTPClient: server.Client()})

	download, err := client.GetAttachment(context.Background(), "received-1", "attachment-1")
	if err != nil {
		t.Fatal(err)
	}
	if download.ID != "attachment-1" || download.DownloadURL != "https://download.example.com/file" || download.Filename != "report.pdf" || download.ContentType != "application/pdf" {
		t.Fatalf("download=%+v", download)
	}
}
