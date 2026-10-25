package api

// oidc_test.go: the verifier is tested against real RS256 signatures and the
// attacks that actually get past naive JWT code — alg "none", the public key
// used as an HMAC secret, a foreign key, and a stale key set.

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
)

const testKid = "key-1"

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwksServer publishes one RSA public key and counts fetches.
type jwksServer struct {
	srv     *httptest.Server
	fetches int
	key     *rsa.PrivateKey
	kid     string
}

func newJWKS(t *testing.T, key *rsa.PrivateKey, kid string) *jwksServer {
	t.Helper()
	j := &jwksServer{key: key, kid: kid}
	j.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		j.fetches++
		pub := &key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
				"n": b64(pub.N.Bytes()),
				"e": b64(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	}))
	t.Cleanup(j.srv.Close)
	return j
}

func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	payload, _ := json.Marshal(claims)
	signing := b64(header) + "." + b64(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signing + "." + b64(sig)
}

func validClaims(iss, aud string) map[string]any {
	return map[string]any{
		"iss": iss, "aud": aud, "sub": "user-123",
		"email": "alice@example.com",
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
}

func verifierFor(t *testing.T) (*oidcVerifier, *rsa.PrivateKey, *jwksServer) {
	t.Helper()
	key := newRSAKey(t)
	jwks := newJWKS(t, key, testKid)
	v := newOIDCVerifier(OIDCConfig{
		Issuer: "https://idp.example.com", Audience: "alpacaruns", JWKSURL: jwks.srv.URL,
	})
	return v, key, jwks
}

func TestOIDCVerifiesValidToken(t *testing.T) {
	v, key, _ := verifierFor(t)
	tok := signToken(t, key, testKid, validClaims("https://idp.example.com", "alpacaruns"))
	who, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if who != "alice@example.com" {
		t.Fatalf("actor = %q, want the email", who)
	}
	// The key set is cached, not refetched per request.
	jwks := v.keys
	if len(jwks) != 1 {
		t.Fatalf("expected one cached key, got %d", len(jwks))
	}
}

func TestOIDCRejectsAlgNone(t *testing.T) {
	v, _, _ := verifierFor(t)
	header, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT", "kid": testKid})
	payload, _ := json.Marshal(validClaims("https://idp.example.com", "alpacaruns"))
	tok := b64(header) + "." + b64(payload) + "."
	if _, err := v.Verify(context.Background(), tok); err == nil {
		t.Fatal("alg=none was accepted — signature verification is bypassable")
	} else if !strings.Contains(err.Error(), "RS256") {
		t.Fatalf("unexpected rejection reason: %v", err)
	}
}

// The public key is public. If the verifier accepted HMAC, anyone who fetched
// the JWKS could mint tokens with the public key as the secret.
func TestOIDCRejectsHMACWithPublicKey(t *testing.T) {
	v, key, _ := verifierFor(t)
	header, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT", "kid": testKid})
	payload, _ := json.Marshal(validClaims("https://idp.example.com", "alpacaruns"))
	signing := b64(header) + "." + b64(payload)
	mac := hmacSHA256(key.PublicKey.N.Bytes(), []byte(signing))
	if _, err := v.Verify(context.Background(), signing+"."+b64(mac)); err == nil {
		t.Fatal("HS256 signed with the public key was accepted")
	}
}

func TestOIDCRejectsWrongIssuerAudienceAndExpiry(t *testing.T) {
	v, key, _ := verifierFor(t)
	cases := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"wrong issuer", validClaims("https://evil.example.com", "alpacaruns"), "issuer"},
		{"wrong audience", validClaims("https://idp.example.com", "another-app"), "audience"},
		{"expired", func() map[string]any {
			c := validClaims("https://idp.example.com", "alpacaruns")
			c["exp"] = time.Now().Add(-2 * time.Hour).Unix()
			return c
		}(), "expired"},
		{"no expiry", func() map[string]any {
			c := validClaims("https://idp.example.com", "alpacaruns")
			delete(c, "exp")
			return c
		}(), "expiry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := signToken(t, key, testKid, tc.claims)
			if _, err := v.Verify(context.Background(), tok); err == nil {
				t.Fatalf("%s: token accepted", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: rejection did not name the reason: %v", tc.name, err)
			}
		})
	}
}

func TestOIDCRejectsForeignKeyAndUnknownKid(t *testing.T) {
	v, _, _ := verifierFor(t)
	other := newRSAKey(t)
	if _, err := v.Verify(context.Background(), signToken(t, other, testKid,
		validClaims("https://idp.example.com", "alpacaruns"))); err == nil {
		t.Fatal("a token signed by another key was accepted")
	} else if !strings.Contains(err.Error(), "signature") {
		t.Fatalf("unexpected rejection: %v", err)
	}
	// Unknown kid: one refetch, then refuse. Rotation must not mean an hour
	// of lockout, and an unknown kid must not mean "trust it".
	if _, err := v.Verify(context.Background(), signToken(t, other, "rotated-key",
		validClaims("https://idp.example.com", "alpacaruns"))); err == nil {
		t.Fatal("a token with an unknown key id was accepted")
	}
}

func TestOIDCNotConfiguredRefuses(t *testing.T) {
	v := newOIDCVerifier(OIDCConfig{})
	if _, err := v.Verify(context.Background(), "a.b.c"); err == nil {
		t.Fatal("an unconfigured verifier accepted a token")
	}
}

// The server resolves an OIDC actor through the same path as named tokens,
// and a rejected JWT does not fall through to a static-token match.
func TestActorPrefersOIDCAndDoesNotFallThrough(t *testing.T) {
	key := newRSAKey(t)
	jwks := newJWKS(t, key, testKid)
	s := newTestServer(t, t.TempDir())
	s.settings.AuthToken = "shared"
	s.cfg = &config.Config{
		OIDCIssuer: "https://idp.example.com", OIDCAudience: "alpacaruns", OIDCJWKSURL: jwks.srv.URL,
		DashboardTokens: map[string]string{"alice": "shared"},
	}
	s.oidc = newOIDCVerifier(OIDCConfig{
		Issuer: s.cfg.OIDCIssuer, Audience: s.cfg.OIDCAudience, JWKSURL: s.cfg.OIDCJWKSURL,
	})

	good := signToken(t, key, testKid, validClaims("https://idp.example.com", "alpacaruns"))
	if got := s.actorFor(withBearer(good)); got != "oidc:alice@example.com" {
		t.Fatalf("OIDC actor = %q", got)
	}
	// A bad JWT must be refused outright; it must not be compared against the
	// static tokens as a string.
	bad := signToken(t, key, testKid, validClaims("https://evil.example.com", "alpacaruns"))
	if got := s.actorFor(withBearer(bad)); got != "" {
		t.Fatalf("a rejected JWT was accepted as %q", got)
	}
	// Non-JWT static tokens still work.
	if got := s.actorFor(withBearer("shared")); got != "operator" {
		t.Fatalf("static token path broken: %q", got)
	}
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}
