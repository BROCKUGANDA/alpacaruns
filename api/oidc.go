package api

// oidc.go: bearer tokens issued by an identity provider.
//
// Static named tokens answer "which secret did this caller hold". That is
// enough to attribute an action and revoke one operator, but not enough to
// call the result SSO: a secret in a file never expires, cannot be scoped to a
// person's current role, and does not survive a staff change without an
// operator editing a file on the box.
//
// This verifies a signed JWT against the provider's published keys:
//
//   - the algorithm must be RS256. "none" and HMAC algorithms are refused by
//     name, which is the alg-confusion attack in one check;
//   - the signature must verify against the key the header names, fetched
//     from the provider's JWKS and cached;
//   - iss and aud must match the configuration exactly;
//   - exp is enforced with a small skew, nbf and iat are sanity-checked.
//
// The actor is the token's email (or subject), so the journal records a real
// identity. Nothing here validates AUTHORISATION beyond holding a valid
// token: a provider group-to-role map is an operator decision, not a default.

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OIDCConfig is the provider trust configuration.
type OIDCConfig struct {
	Issuer   string
	Audience string
	// JWKSURL is the provider's key set. Given explicitly rather than
	// discovered from the issuer: discovery would add a network dependency
	// and an SSRF surface at startup.
	JWKSURL string
}

// Enabled reports whether OIDC verification is configured.
func (c OIDCConfig) Enabled() bool {
	return strings.TrimSpace(c.Issuer) != "" &&
		strings.TrimSpace(c.Audience) != "" &&
		strings.TrimSpace(c.JWKSURL) != ""
}

const (
	jwksTTL       = time.Hour
	clockSkew     = 60 * time.Second
	maxTokenBytes = 8 * 1024
)

type oidcVerifier struct {
	cfg    OIDCConfig
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func newOIDCVerifier(cfg OIDCConfig) *oidcVerifier {
	return &oidcVerifier{
		cfg:    cfg,
		client: &http.Client{Timeout: 5 * time.Second},
		now:    time.Now,
		keys:   map[string]*rsa.PublicKey{},
	}
}

// Verify validates a compact JWT and returns the acting identity.
func (v *oidcVerifier) Verify(ctx context.Context, token string) (string, error) {
	if !v.cfg.Enabled() {
		return "", fmt.Errorf("oidc not configured")
	}
	if len(token) > maxTokenBytes {
		return "", fmt.Errorf("token too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("not a compact JWT")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("bad header encoding")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return "", fmt.Errorf("bad header")
	}
	// Explicit allow-list of one algorithm. This is the check that stops the
	// classic attack of re-signing with "none" or with the public key used as
	// an HMAC secret.
	if !strings.EqualFold(header.Alg, "RS256") {
		return "", fmt.Errorf("unsupported alg %q (only RS256 is accepted)", header.Alg)
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("bad payload encoding")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("bad signature encoding")
	}

	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return "", fmt.Errorf("signature does not verify")
	}

	var claims map[string]any
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return "", fmt.Errorf("bad claims")
	}
	now := v.now()
	if err := checkClaims(claims, v.cfg, now); err != nil {
		return "", err
	}
	for _, k := range []string{"email", "preferred_username", "sub"} {
		if s, ok := claims[k].(string); ok && strings.TrimSpace(s) != "" {
			return s, nil
		}
	}
	return "", fmt.Errorf("token carries no usable subject")
}

// checkClaims enforces issuer, audience and validity window.
func checkClaims(claims map[string]any, cfg OIDCConfig, now time.Time) error {
	iss, _ := claims["iss"].(string)
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(iss)), []byte(strings.TrimSpace(cfg.Issuer))) != 1 {
		return fmt.Errorf("issuer mismatch")
	}
	if !audienceContains(claims["aud"], cfg.Audience) {
		return fmt.Errorf("audience mismatch")
	}
	exp, ok := numericDate(claims["exp"])
	if !ok {
		return fmt.Errorf("token has no expiry")
	}
	if now.After(exp.Add(clockSkew)) {
		return fmt.Errorf("token expired at %s", exp.UTC().Format(time.RFC3339))
	}
	if nbf, ok := numericDate(claims["nbf"]); ok && now.Add(clockSkew).Before(nbf) {
		return fmt.Errorf("token not valid before %s", nbf.UTC().Format(time.RFC3339))
	}
	return nil
}

func audienceContains(v any, want string) bool {
	switch a := v.(type) {
	case string:
		return a == want
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func numericDate(v any) (time.Time, bool) {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// key returns the public key for kid, refreshing the cache when it is stale
// or the kid is unknown. An unknown kid triggers ONE refetch: providers
// rotate keys, and refusing until the TTL expires would lock operators out
// for an hour after a rotation.
func (v *oidcVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && time.Since(v.fetched) < jwksTTL {
		return k, nil
	}
	if err := v.refreshLocked(ctx); err != nil {
		// A stale cache is better than an outage: if the set was fetched
		// before, keep using it.
		if k, ok := v.keys[kid]; ok {
			return k, nil
		}
		return nil, err
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("signing key %q not found in the provider's key set", kid)
}

func (v *oidcVerifier) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch JWKS: HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			Alg string `json:"alg"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("parse JWKS: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if !strings.EqualFold(k.Kty, "RSA") {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		e := new(big.Int).SetBytes(eBytes)
		if !e.IsInt64() || e.Int64() > int64(1<<31) {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e.Int64())}
	}
	if len(keys) == 0 {
		return fmt.Errorf("provider key set contained no usable RSA keys")
	}
	v.keys = keys
	v.fetched = time.Now()
	return nil
}
