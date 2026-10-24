package agents

// untrusted.go: everything an agent reads that a third party wrote — news
// articles, tool output, peer messages — is DATA, and must reach the model
// looking like data.
//
// This is defence in depth, not a guarantee. A prompt cannot be made
// injection-proof by punctuation; what this file does is make the boundary
// explicit (one envelope, one label), bound how much of it can be consumed,
// and raise the cost of a message that tries to look like an instruction.

import (
	"strings"
	"unicode"
)

// maxUntrustedLen bounds how much third-party text is passed through. An
// unbounded paste is a budget attack as much as an injection one.
const maxUntrustedLen = 8000

// injectionMarkers are phrases that only appear in text trying to redirect
// the reader. They are logged, not stripped: silently rewriting evidence is
// worse than flagging it.
var injectionMarkers = []string{
	"ignore previous", "ignore all previous", "disregard previous",
	"system prompt", "you are now", "new instructions", "override instructions",
	"forget your", "act as", "reveal your", "execute the following",
}

// WrapUntrusted puts third-party text inside a labelled envelope after
// stripping control characters and capping its length.
//
// The label travels with the content, so a later reader (human or model) can
// tell a quoted headline from a line the system wrote.
func WrapUntrusted(label, text string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "untrusted"
	}
	clean := sanitizeUntrusted(text)
	if clean == "" {
		return ""
	}
	return "<untrusted:" + label + ">" + clean + "</untrusted:" + label + ">"
}

// sanitizeUntrusted removes control characters (including the escape that
// erases a terminal line) and caps length.
func sanitizeUntrusted(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r):
			continue
		default:
			b.WriteRune(r)
		}
		if b.Len() >= maxUntrustedLen {
			b.WriteString("…[truncated]")
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// LooksLikeInjection reports whether text carries a phrase characteristic of
// an instruction aimed at the reader rather than a description of the market.
func LooksLikeInjection(text string) bool {
	lower := strings.ToLower(text)
	for _, m := range injectionMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
