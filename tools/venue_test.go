package tools

// venue_test.go: the venue proof reads the ACCOUNT, not the URL string.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func venueClient(t *testing.T, account map[string]any, status int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(account)
	}))
	t.Cleanup(srv.Close)
	return NewClient("k", "s", srv.URL, srv.URL)
}

// A paper-looking URL with credentials that resolve to a live-format
// account is exactly the failure mode a URL check cannot see.
func TestVerifyVenueRefusesLiveAccountUnderPaperURL(t *testing.T) {
	c := venueClient(t, map[string]any{
		"id": "1f2e3d4c-0000-4000-8000-000000000000", "account_number": "123456789",
		"status": "ACTIVE",
	}, http.StatusOK)
	if _, err := c.VerifyVenue(context.Background(), true); err == nil {
		t.Fatal("a live-format account was accepted as the paper account")
	} else if !strings.Contains(err.Error(), "does not look like a paper account") {
		t.Fatalf("refusal did not explain itself: %v", err)
	}
}

func TestVerifyVenueAcceptsPaperAccount(t *testing.T) {
	c := venueClient(t, map[string]any{
		"id": "PA31ZU4MBVRN", "account_number": "PA31ZU4MBVRN", "status": "ACTIVE",
	}, http.StatusOK)
	a, err := c.VerifyVenue(context.Background(), true)
	if err != nil {
		t.Fatalf("paper account rejected: %v", err)
	}
	if a.ID != "PA31ZU4MBVRN" {
		t.Fatalf("returned the wrong account: %+v", a)
	}
}

// Live mode is gated elsewhere (URL + explicit acknowledgement); the proof
// does not second-guess it, but it still reports the account.
func TestVerifyVenueLiveModeSkipsPaperPrefix(t *testing.T) {
	c := venueClient(t, map[string]any{"id": "abc", "status": "ACTIVE"}, http.StatusOK)
	if _, err := c.VerifyVenue(context.Background(), false); err != nil {
		t.Fatalf("live mode verification should not require a paper id: %v", err)
	}
}

// An unidentified account proves nothing, and an unreadable one proves
// less.
func TestVerifyVenueFailsClosedWithoutEvidence(t *testing.T) {
	blank := venueClient(t, map[string]any{"status": "ACTIVE"}, http.StatusOK)
	if _, err := blank.VerifyVenue(context.Background(), true); err == nil {
		t.Fatal("an account with no id was accepted as paper")
	}
	broken := venueClient(t, map[string]any{"message": "nope"}, http.StatusInternalServerError)
	if _, err := broken.VerifyVenue(context.Background(), true); err == nil {
		t.Fatal("an unreadable account was accepted as paper")
	}
	var nilClient *Client
	if _, err := nilClient.VerifyVenue(context.Background(), true); err == nil {
		t.Fatal("a nil client verified a venue")
	}
}

func TestExpectPaperVenue(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://paper-api.alpaca.markets/v2", true},
		{"https://PAPER-API.alpaca.markets", true},
		{"https://api.alpaca.markets/v2", false},
		{"", false},
	} {
		if got := ExpectPaperVenue(tc.url); got != tc.want {
			t.Errorf("ExpectPaperVenue(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
