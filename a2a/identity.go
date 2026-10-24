package a2a

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
)

// Fingerprint hashes the parts of a signal that decide WHAT would be
// traded: agent, symbol, side, size and confidence.
//
// Dedupe keyed on the id alone is not idempotency — it is amnesia. A
// peer that replays a used id with a different symbol, side or size is
// not sending the same signal twice, and silently dropping that hides
// either an authoring bug or an attempt to smuggle a second order under
// an already-blessed id. Same id plus a different fingerprint is
// therefore refused out loud.
//
// The free-text reason is deliberately left out: it cannot change what
// is traded, and binding it in would turn an edited comment into a
// reuse incident.
func Fingerprint(sig Signal) string {
	h := sha256.New()
	for _, field := range []string{
		strings.ToLower(strings.TrimSpace(sig.Agent)),
		strings.ToUpper(strings.TrimSpace(sig.Symbol)),
		strings.ToLower(strings.TrimSpace(sig.Side)),
		strings.TrimSpace(sig.Qty),
		strings.TrimSpace(sig.Notional),
		strconv.FormatFloat(sig.Confidence, 'f', -1, 64),
	} {
		// Length-prefix every field so "AB"+"C" and "A"+"BC" cannot
		// produce the same byte stream.
		h.Write([]byte(strconv.Itoa(len(field))))
		h.Write([]byte{':'})
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hasCredential reports whether the bridge has any way to authenticate a
// sender. No credential means no ingress: there is no anonymous mode.
func (c Config) hasCredential() bool {
	if strings.TrimSpace(c.Token) != "" {
		return true
	}
	for _, token := range c.PeerTokens {
		if strings.TrimSpace(token) != "" {
			return true
		}
	}
	return false
}

// tokenFor returns the credential bound to one agent id and reports
// whether the agent has its own. Matching is case-insensitive and
// whitespace-tolerant, like the allow-list. An entry whose token is
// blank is not a credential, so it does not lock the peer out: the
// group token still applies to that agent.
func (c Config) tokenFor(agent string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(agent))
	if want == "" {
		return "", false
	}
	for id, token := range c.PeerTokens {
		if strings.ToLower(strings.TrimSpace(id)) != want {
			continue
		}
		if strings.TrimSpace(token) == "" {
			return "", false
		}
		return token, true
	}
	return "", false
}

// Authenticate reports whether bearer is the credential for a signal
// claiming to come from agent.
//
// The bridge token authenticates the sender, so an agent id asserted in
// the body may not be the whole story. When the claimed agent has a peer
// token of its own, ONLY that token may post as it — the shared group
// token cannot impersonate it. An agent with no token of its own keeps
// using the group token, so a deployment that has never configured
// PeerTokens behaves exactly as it does today.
//
// Every comparison is constant-time: neither path may leak how close a
// guess was.
func (c Config) Authenticate(agent, bearer string) bool {
	if bound, ok := c.tokenFor(agent); ok {
		return constantTimeEqual(bearer, bound)
	}
	return constantTimeEqual(bearer, c.Token)
}

// MatchToken is the bridge's view of Authenticate, so the caller's
// credential rules and the ingress checks come from one configuration.
func (b *Bridge) MatchToken(agent, bearer string) bool {
	return b.cfg.Authenticate(agent, bearer)
}

// MatchAnyToken reports whether bearer is SOME credential this bridge
// accepts — the group token or any peer's own token. It answers "is this
// caller a peer at all?" before the body has been parsed; the per-agent
// binding is checked once the claimed agent is known.
func (c Config) MatchAnyToken(bearer string) bool {
	got := strings.TrimSpace(bearer)
	if got == "" {
		return false
	}
	if constantTimeEqual(got, c.Token) {
		return true
	}
	for _, token := range c.PeerTokens {
		if constantTimeEqual(got, token) {
			return true
		}
	}
	return false
}

// constantTimeEqual compares two secrets without timing, and never
// treats a blank side as a match (empty slices compare equal otherwise).
func constantTimeEqual(a, b string) bool {
	x, y := strings.TrimSpace(a), strings.TrimSpace(b)
	if x == "" || y == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(x), []byte(y)) == 1
}
