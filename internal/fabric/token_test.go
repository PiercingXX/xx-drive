package fabric

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var keyID = "00aa00bb00cc00dd"
var secret = bytes.Repeat([]byte{0x5a}, 32)
var now = time.Unix(1700000000, 0)

func mint(userID string, secret []byte, kid string, exp float64) string {
	claims := map[string]any{"user_id": userID, "jti": "t", "iat": 1700000000.0, "exp": exp}
	payload, _ := json.Marshal(claims)
	body := base64.RawURLEncoding.EncodeToString(payload)
	signed := "v1." + kid + "." + body
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// TestLoadKeyringAndValidate exercises the exact deploy path: a ring JSON file
// (as ClusterKeyring.save writes it) loaded via LoadKeyring, then a token
// minted in the documented format validates to its user_id.
func TestLoadKeyringAndValidate(t *testing.T) {
	dir := t.TempDir()
	ringPath := filepath.Join(dir, "fabric-keys.json")
	ring := map[string]any{"keys": map[string]string{keyID: "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"}, "active_key_id": keyID}
	raw, _ := json.Marshal(ring)
	if err := os.WriteFile(ringPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := LoadKeyring(ringPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	valid := 1700000000.0 + 3600
	uid, err := k.UserIDFor("Bearer "+mint("user-123", secret, keyID, valid), now)
	if err != nil || uid != "user-123" {
		t.Fatalf("validate: uid=%q err=%v", uid, err)
	}
	// wrong key rejected
	if _, err := k.UserIDFor("Bearer "+mint("user-123", secret, "deadbeefdeadbeef", valid), now); err != ErrAuth {
		t.Fatalf("wrong key: expected ErrAuth, got %v", err)
	}
	// expired rejected
	if _, err := k.UserIDFor("Bearer "+mint("user-123", secret, keyID, 1699999999.0), now); err != ErrAuth {
		t.Fatalf("expired: expected ErrAuth, got %v", err)
	}
}

func TestLoadKeyringMissing(t *testing.T) {
	if _, err := LoadKeyring(""); err == nil {
		t.Fatal("expected error for empty/unconfigured keyring path")
	}
}
