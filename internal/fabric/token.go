// Package fabric validates estate SSO bearer tokens locally, with no call back
// to xx-chat. It is the Go sibling of skippy-tel-network's
// syncdaemon/fabric_auth.py (the validate-only helper) and reproduces the
// ClusterKeyring **v1** token format byte-for-byte from
// syncdaemon/session_keys.py so a token minted by xx-chat's
// POST /api/v1/fabric/login validates here statelessly. Shared verbatim with
// xx-note's validator (server/internal/fabric/token.go) so every estate app
// accepts one and the same token.
//
// Token format (version 1):
//
//	v1.<key_id>.<b64url(claims_json)>.<b64url(hmac_sha256(secret, signed_part))>
//
// where signed_part is "v1.<key_id>.<b64url(claims_json)>". The b64url encoding
// is URL-safe base64 with padding stripped (Python's
// base64.urlsafe_b64encode(...).rstrip(b"=")), i.e. Go's base64.RawURLEncoding.
// The claims JSON carries user_id, jti, iat and exp (epoch seconds).
//
// The signing key material is the operator-provisioned cluster keyring — the
// same owner-only JSON file ClusterKeyring.save writes:
//
//	{"keys": {"<key_id>": "<secret_hex>", ...}, "active_key_id": "<key_id>"}
//
// located via FABRIC_CLUSTER_KEYS_PATH. This node is validate-only: it never
// signs and never needs an active key, only the ring of accepted keys.
//
// Only the Go standard library is used.
package fabric

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

// EnvKeyringPath is the env var naming the operator-provisioned ring file —
// the same name the Python mint/validate sides read, so one config value names
// the shared ring everywhere in the estate.
const EnvKeyringPath = "FABRIC_CLUSTER_KEYS_PATH"

const tokenVersion = "v1"

// ErrAuth is returned for any presented credential that is missing, malformed,
// expired, or not signed by a key this node accepts. The message is deliberately
// coarse — no account-state or key oracle leaks to the caller.
var ErrAuth = errors.New("token is invalid, expired, or signed by an unaccepted key")

// Keyring is one node's set of accepted cluster keys: key_id -> secret bytes.
// Validate-only, so unlike the Python ClusterKeyring it carries no active key.
type Keyring struct {
	keys map[string][]byte
}

// NewKeyring builds a ring from an already-decoded key map (used by tests).
func NewKeyring(keys map[string][]byte) *Keyring {
	m := make(map[string][]byte, len(keys))
	for k, v := range keys {
		m[k] = v
	}
	return &Keyring{keys: m}
}

// ringFile mirrors the JSON ClusterKeyring.save writes.
type ringFile struct {
	Keys        map[string]string `json:"keys"`
	ActiveKeyID string            `json:"active_key_id"`
}

// LoadKeyring reads the validate-capable ring from path, or from the
// FABRIC_CLUSTER_KEYS_PATH env var when path is empty. A missing path or an
// unreadable/malformed ring is a loud error — call this once at startup so a
// misconfigured node fails to boot, not on the first request. A validate-only
// consumer may hold a ring with no active_key_id (keys installed but never
// activated); it still validates every token signed by a key in the ring.
func LoadKeyring(path string) (*Keyring, error) {
	if path == "" {
		path = os.Getenv(EnvKeyringPath)
	}
	if path == "" {
		return nil, errors.New("no fabric cluster keyring configured (set " + EnvKeyringPath + ")")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rf ringFile
	if err := json.Unmarshal(raw, &rf); err != nil {
		return nil, err
	}
	if len(rf.Keys) == 0 {
		return nil, errors.New("fabric keyring holds no keys")
	}
	keys := make(map[string][]byte, len(rf.Keys))
	for kid, hexed := range rf.Keys {
		secret, err := hex.DecodeString(hexed)
		if err != nil {
			return nil, errors.New("fabric keyring key " + kid + " is not valid hex")
		}
		keys[kid] = secret
	}
	return &Keyring{keys: keys}, nil
}

// Claims are the token payload fields this validator reads.
type Claims struct {
	UserID string  `json:"user_id"`
	JTI    string  `json:"jti"`
	IAT    float64 `json:"iat"`
	EXP    float64 `json:"exp"`
}

// unb64url decodes the URL-safe, padding-stripped base64 the Python side emits.
// It tolerates a padded value too, for robustness against a re-encoding proxy.
func unb64url(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// Validate checks a raw token against the ring and returns its claims, or an
// error. It is the Go equivalent of ClusterKeyring.validate: split into the
// four dot-separated parts, look up the signing key by id, recompute the HMAC
// over the exact signed_part string, constant-time compare, then parse claims
// and enforce expiry. now is the moment to test exp against (time.Now used when
// zero) — a token is expired when now >= exp, matching the Python `moment >= exp`.
func (k *Keyring) Validate(token string, now time.Time) (Claims, error) {
	var c Claims
	if token == "" {
		return c, ErrAuth
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != tokenVersion {
		return c, ErrAuth
	}
	keyID, body, sig := parts[1], parts[2], parts[3]
	secret, ok := k.keys[keyID]
	if !ok {
		return c, ErrAuth
	}
	signedPart := tokenVersion + "." + keyID + "." + body
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signedPart))
	expected := mac.Sum(nil)
	presented, err := unb64url(sig)
	if err != nil {
		return c, ErrAuth
	}
	if !hmac.Equal(expected, presented) {
		return c, ErrAuth
	}
	payload, err := unb64url(body)
	if err != nil {
		return c, ErrAuth
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, ErrAuth
	}
	if c.EXP == 0 {
		return c, ErrAuth
	}
	moment := now
	if moment.IsZero() {
		moment = time.Now()
	}
	// exp is epoch seconds; compare in the same float domain the Python side uses.
	if float64(moment.UnixNano())/1e9 >= c.EXP {
		return c, ErrAuth
	}
	if c.UserID == "" {
		return c, ErrAuth
	}
	return c, nil
}

// UserIDFor extracts a Bearer token from an Authorization header value and
// returns its validated user_id, or ErrAuth. The user_id it returns is the ONLY
// source of fabric caller identity — never trust a client-supplied id.
func (k *Keyring) UserIDFor(authHeader string, now time.Time) (string, error) {
	h := strings.TrimSpace(authHeader)
	if h == "" {
		return "", ErrAuth
	}
	fields := strings.Fields(h)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") || fields[1] == "" {
		return "", ErrAuth
	}
	c, err := k.Validate(fields[1], now)
	if err != nil {
		return "", err
	}
	return c.UserID, nil
}

// LooksLikeToken reports whether raw has the shape of a ClusterKeyring v1 token
// (v1.<key>.<body>.<sig>). It is a cheap dispatch aid so the server can route a
// bearer credential to fabric validation vs. a local opaque session token
// (which never contains dots). It makes NO trust decision — Validate does.
func LooksLikeToken(raw string) bool {
	parts := strings.Split(raw, ".")
	return len(parts) == 4 && parts[0] == tokenVersion
}
