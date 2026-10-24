package config

import "testing"

func TestParsePeerTokens(t *testing.T) {
	got, err := parsePeerTokens("research:r-secret, analyst : a-secret ")
	if err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if got["research"] != "r-secret" || got["analyst"] != "a-secret" {
		t.Fatalf("parsed wrong: %+v", got)
	}
	if m, err := parsePeerTokens(""); err != nil || m != nil {
		t.Fatalf("empty input should be nil,nil: %+v %v", m, err)
	}
	// Anything malformed is an error: a peer the operator believes is bound
	// to its own token but silently is not would keep accepting the group
	// token.
	for _, bad := range []string{"research", "research:", ":r", "research:r,research:r2"} {
		if _, err := parsePeerTokens(bad); err == nil {
			t.Errorf("malformed %q was accepted", bad)
		}
	}
}
