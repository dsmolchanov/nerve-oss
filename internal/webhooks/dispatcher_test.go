package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestSignPayload_Deterministic(t *testing.T) {
	secret := "top-secret"
	ts := "1700000000"
	body := []byte(`{"event_type":"email.delivered"}`)

	got := SignPayload(secret, ts, body)

	// Independently compute the expected signature.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Errorf("SignPayload mismatch\ngot:  %s\nwant: %s", got, want)
	}
	if len(got) != 64 {
		t.Errorf("SHA-256 hex signature should be 64 chars, got %d", len(got))
	}
}

func TestSignPayload_BodyBinding(t *testing.T) {
	// Same timestamp + secret but different body should produce a
	// different signature. Prevents a naive implementation that
	// signs only the timestamp.
	secret := "s"
	ts := "1"
	a := SignPayload(secret, ts, []byte("alpha"))
	b := SignPayload(secret, ts, []byte("beta"))
	if a == b {
		t.Error("different bodies must produce different signatures")
	}
}

func TestSignPayload_TimestampBinding(t *testing.T) {
	secret := "s"
	body := []byte("fixed")
	a := SignPayload(secret, "100", body)
	b := SignPayload(secret, "101", body)
	if a == b {
		t.Error("different timestamps must produce different signatures")
	}
}

func TestBackoffForAttempt(t *testing.T) {
	d := &Dispatcher{}
	d.BaseBackoff = 10 // small unit so the math is obvious
	d.MaxBackoff = 10000

	cases := []struct {
		attempt int
		want    int64
	}{
		{0, 10},     // first failure reuses base
		{1, 10},     // attempt 1 → base * 1<<0 = 10
		{2, 20},     // base * 1<<1
		{3, 40},     // base * 1<<2
		{4, 80},     // base * 1<<3
		{10, 5120},  // base * 1<<9
		{20, 10000}, // capped at MaxBackoff
	}
	for _, c := range cases {
		got := int64(d.backoffForAttempt(c.attempt))
		if got != c.want {
			t.Errorf("backoffForAttempt(%d) = %d, want %d", c.attempt, got, c.want)
		}
	}
}
