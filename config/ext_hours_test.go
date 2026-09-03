package config

import (
	"os"
	"strings"
	"testing"
)

// TestExtendedHoursDefault covers the EXTENDED_HOURS env parsing.
func TestExtendedHoursDefault(t *testing.T) {
	t.Setenv("ALPACA_API_KEY_ID", "k")
	t.Setenv("ALPACA_SECRET_KEY", "s")
	os.Unsetenv("EXTENDED_HOURS")

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.ExtendedHours {
		t.Fatal("EXTENDED_HOURS must default to false")
	}

	os.Setenv("EXTENDED_HOURS", "true")
	t.Cleanup(func() { os.Unsetenv("EXTENDED_HOURS") })
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !c.ExtendedHours {
		t.Fatal("EXTENDED_HOURS=true not picked up")
	}

	os.Setenv("EXTENDED_HOURS", "1")
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !c.ExtendedHours {
		t.Fatal("EXTENDED_HOURS=1 not accepted as true")
	}
}

// TestExtendedHoursInvalid verifies strict boolean parsing: a garbage
// value must fail Load, matching getInt/getFloat error style.
func TestExtendedHoursInvalid(t *testing.T) {
	t.Setenv("ALPACA_API_KEY_ID", "k")
	t.Setenv("ALPACA_SECRET_KEY", "s")
	t.Setenv("EXTENDED_HOURS", "maybe")
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error for invalid EXTENDED_HOURS value")
	}
	if !strings.Contains(err.Error(), "not a valid boolean") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestStreamFeedValidation covers STREAM_FEED parsing: iex default,
// sip accepted, anything else rejected.
func TestStreamFeedValidation(t *testing.T) {
	t.Setenv("ALPACA_API_KEY_ID", "k")
	t.Setenv("ALPACA_SECRET_KEY", "s")
	os.Unsetenv("STREAM_FEED")
	t.Cleanup(func() { os.Unsetenv("STREAM_FEED") })

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.StreamFeed != "iex" {
		t.Fatalf("STREAM_FEED default = %q, want iex", c.StreamFeed)
	}

	os.Setenv("STREAM_FEED", "SIP") // case-insensitive
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.StreamFeed != "sip" {
		t.Fatalf("STREAM_FEED=sip not picked up: %q", c.StreamFeed)
	}

	os.Setenv("STREAM_FEED", "polygon")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error for invalid STREAM_FEED")
	}
}
