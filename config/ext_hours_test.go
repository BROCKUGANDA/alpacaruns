package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestExtendedHoursDefault covers the EXTENDED_HOURS env parsing. The session
// is unusable without a liquidity bar, so these cases set one — see
// TestExtendedHoursRequiresSpreadCap for the rule itself.
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

	t.Setenv("MAX_SPREAD_BPS", "25")
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

// TestExtendedHoursRequiresSpreadCap pins the pairing rule: enabling the
// session with no spread bar in either key is refused at load, not discovered
// at the first thin quote.
func TestExtendedHoursRequiresSpreadCap(t *testing.T) {
	t.Setenv("ALPACA_API_KEY_ID", "k")
	t.Setenv("ALPACA_SECRET_KEY", "s")
	t.Setenv("EXTENDED_HOURS", "true")
	t.Setenv("MAX_SPREAD_BPS", "0")
	t.Setenv("EXTENDED_HOURS_MAX_SPREAD_BPS", "0")
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "MAX_SPREAD_BPS") {
		t.Fatalf("want the spread-cap pairing error, got %v", err)
	}
	// The session-specific cap satisfies it on its own.
	t.Setenv("EXTENDED_HOURS_MAX_SPREAD_BPS", "40")
	c, err := Load("")
	if err != nil {
		t.Fatalf("session-specific cap should satisfy the rule: %v", err)
	}
	if got := c.ExtendedSpreadCapBps(); got != 40 {
		t.Fatalf("ExtendedSpreadCapBps() = %g, want 40 (session cap wins)", got)
	}
	// And with no session cap it inherits the general bar.
	t.Setenv("EXTENDED_HOURS_MAX_SPREAD_BPS", "0")
	t.Setenv("MAX_SPREAD_BPS", "18")
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ExtendedSpreadCapBps(); got != 18 {
		t.Fatalf("ExtendedSpreadCapBps() = %g, want 18 (fall back to MAX_SPREAD_BPS)", got)
	}
}

// TestExtendedHoursWindowsAndAge pins the two remaining session knobs: the
// window spec is validated at load, and the quote-age default is the one the
// executor relies on.
func TestExtendedHoursWindowsAndAge(t *testing.T) {
	t.Setenv("ALPACA_API_KEY_ID", "k")
	t.Setenv("ALPACA_SECRET_KEY", "s")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.ExtendedHoursWindows != "04:30-09:25,16:05-19:55" {
		t.Fatalf("EXTENDED_HOURS_WINDOWS default = %q", c.ExtendedHoursWindows)
	}
	if c.MaxQuoteAge() != 5*time.Minute {
		t.Fatalf("MaxQuoteAge() default = %s, want 5m", c.MaxQuoteAge())
	}

	t.Setenv("EXTENDED_HOURS_MAX_QUOTE_AGE_SECONDS", "45")
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxQuoteAge() != 45*time.Second {
		t.Fatalf("MaxQuoteAge() = %s, want 45s", c.MaxQuoteAge())
	}

	// An empty value is not a bad spec: it means "unset", and getEnv falls
	// back to the default. The cases below are the ones a typo produces.
	for _, bad := range []string{"04:30", "25:00-26:00", "09:30-04:30", "04:30-09:25,nope", "04:60-09:25"} {
		t.Setenv("EXTENDED_HOURS_WINDOWS", bad)
		if _, err := Load(""); err == nil {
			t.Fatalf("EXTENDED_HOURS_WINDOWS=%q accepted", bad)
		}
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
