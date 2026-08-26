package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xxdrive/internal/fabric"
	"xxdrive/internal/fsdrv"
	"xxdrive/internal/store"
)

// --- test rig reproducing the ClusterKeyring v1 mint side, byte-for-byte ------

var fabKeyID = "00aa00bb00cc00dd"
var fabSecret = bytes.Repeat([]byte{0x5a}, 32)
var fabNow = time.Unix(1700000000, 0)
var fabValidExp = 1700000000.0 + 30*24*3600 // 30 days after fabNow

func mintFabric(userID, keyID string, secret []byte, exp float64) string {
	claims := map[string]any{"user_id": userID, "jti": "t", "iat": 1700000000.0, "exp": exp}
	payload, _ := json.Marshal(claims)
	body := base64.RawURLEncoding.EncodeToString(payload)
	signedPart := "v1." + keyID + "." + body
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signedPart))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signedPart + "." + sig
}

// newFabricEnv builds a server with a real validate-only keyring and a frozen
// clock, so token-expiry is deterministic.
func newFabricEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	fsd, err := fsdrv.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ring := fabric.NewKeyring(map[string][]byte{fabKeyID: fabSecret})
	cfg := Config{Addr: ":0", MaxUploadMB: 8, SessionTTL: time.Hour, TrashRetentionDays: 30,
		nowFunc: func() time.Time { return fabNow }}
	s := New(cfg, st, fsd, ring)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	return &testEnv{t: t, st: st, fs: fsd, srvr: ts, client: ts.Client(), baseURL: ts.URL}
}

// TestFabricTokenAuthenticatesAndMapsToUserID: a token minted in the documented
// format authenticates and its user_id becomes the storage-isolation key.
func TestFabricTokenAuthenticatesAndMapsToUserID(t *testing.T) {
	e := newFabricEnv(t)
	uid := "3f9a1b2c4d5e6f70a1b2c3d4e5f60718"
	tok := mintFabric(uid, fabKeyID, fabSecret, fabValidExp)

	// /api/auth/me reflects the fabric identity.
	resp, b := e.req("GET", "/api/auth/me", tok, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("me: %d %s", resp.StatusCode, b)
	}
	var me struct {
		Username string `json:"username"`
		Fabric   bool   `json:"fabric"`
	}
	json.Unmarshal(b, &me)
	if !me.Fabric {
		t.Fatalf("expected fabric identity, got %s", b)
	}
	if me.Username != "fabric_"+uid {
		t.Fatalf("storage key not derived from user_id: %q", me.Username)
	}

	// An upload lands under files/fabric_<uid>/ — the token's user_id, nothing else.
	resp, _ = e.upload(tok, "/note.txt", "hello estate", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	onDisk := filepath.Join(e.fs.Root(), "files", "fabric_"+uid, "note.txt")
	if _, err := readFile(onDisk); err != nil {
		t.Fatalf("file not isolated under user_id dir: %v", err)
	}
}

// TestFabricInvalidTokensRejected: wrong key, tampered sig, expired, and
// malformed tokens are all 401 — no fallthrough, no oracle.
func TestFabricInvalidTokensRejected(t *testing.T) {
	e := newFabricEnv(t)
	uid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	cases := map[string]string{
		"wrong-key":     mintFabric(uid, "ffffffffffffffff", fabSecret, fabValidExp),
		"wrong-secret":  mintFabric(uid, fabKeyID, bytes.Repeat([]byte{0x11}, 32), fabValidExp),
		"expired":       mintFabric(uid, fabKeyID, fabSecret, 1700000000.0-1),
		"malformed":     "v1.only.three",
		"not-a-token":   "v1." + fabKeyID + ".xxx.yyy",
		"empty-user_id": mintFabric("", fabKeyID, fabSecret, fabValidExp),
	}
	for name, tok := range cases {
		resp, b := e.req("GET", "/api/files/list?path=/", tok, nil, nil)
		if resp.StatusCode != 401 {
			t.Fatalf("%s: expected 401, got %d %s", name, resp.StatusCode, b)
		}
	}

	// A tampered signature on an otherwise-valid token is rejected too.
	valid := mintFabric(uid, fabKeyID, fabSecret, fabValidExp)
	tampered := valid[:len(valid)-2] + "AA"
	resp, _ := e.req("GET", "/api/files/list?path=/", tampered, nil, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("tampered sig: expected 401, got %d", resp.StatusCode)
	}
}

// TestFabricDisabledWithoutKeyring: with no keyring configured, a fabric bearer
// token is refused and the /api/auth/fabric endpoint reports unavailable — but
// local admin auth (nil-ring server) still works via the other tests.
func TestFabricDisabledWithoutKeyring(t *testing.T) {
	e := newTestEnv(t) // nil ring
	tok := mintFabric("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", fabKeyID, fabSecret, fabValidExp)
	resp, _ := e.req("GET", "/api/files/list?path=/", tok, nil, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 with no keyring, got %d", resp.StatusCode)
	}
	body, _ := json.Marshal(map[string]string{"token": tok})
	resp, _ = e.req("POST", "/api/auth/fabric", "", bytes.NewReader(body), jsonHdr())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 fabric-login without keyring, got %d", resp.StatusCode)
	}
}

// TestFabricTwoUserIsolation is THE deliverable proof: under fabric identity,
// user A's token can never read, download, or delete user B's file, and their
// storage keys/dirs are distinct.
func TestFabricTwoUserIsolation(t *testing.T) {
	e := newFabricEnv(t)
	uidA := "11111111111111111111111111111111"
	uidB := "22222222222222222222222222222222"
	tokA := mintFabric(uidA, fabKeyID, fabSecret, fabValidExp)
	tokB := mintFabric(uidB, fabKeyID, fabSecret, fabValidExp)

	// B stores a secret.
	if resp, _ := e.upload(tokB, "/secret.txt", "B-only-content", nil); resp.StatusCode != 200 {
		t.Fatalf("B upload: %d", resp.StatusCode)
	}

	// Distinct storage keys → distinct on-disk roots. This is the isolation
	// proof at the storage layer: A and B can never share a directory.
	uA, err := e.st.GetOrCreateFabricUser(uidA)
	if err != nil {
		t.Fatal(err)
	}
	uB, err := e.st.GetOrCreateFabricUser(uidB)
	if err != nil {
		t.Fatal(err)
	}
	if uA.Username == uB.Username || uA.ID == uB.ID {
		t.Fatalf("isolation broken: A=%v B=%v", uA, uB)
	}

	// A populates its own drive, then lists it: A's file is present, B's is not.
	if resp, _ := e.upload(tokA, "/a-own.txt", "A-owns-this", nil); resp.StatusCode != 200 {
		t.Fatalf("A own upload: %d", resp.StatusCode)
	}
	resp, b := e.req("GET", "/api/files/list?path=/", tokA, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("A list: %d %s", resp.StatusCode, b)
	}
	if !bytes.Contains(b, []byte("a-own.txt")) {
		t.Fatalf("A cannot see its own file: %s", b)
	}
	if bytes.Contains(b, []byte("secret.txt")) {
		t.Fatalf("A saw B's file: %s", b)
	}

	// A cannot download B's file by path (path is A-scoped → not found).
	resp, _ = e.req("GET", "/api/files/download?path="+url.QueryEscape("/secret.txt"), tokA, nil, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("A download of B's file: expected 404, got %d", resp.StatusCode)
	}

	// A cannot delete B's file.
	body, _ := json.Marshal(map[string]string{"path": "/secret.txt"})
	resp, _ = e.req("POST", "/api/files/delete", tokA, bytes.NewReader(body), jsonHdr())
	if resp.StatusCode == 200 {
		t.Fatalf("A deleted B's file (status 200)")
	}

	// B's file survives untouched.
	onDisk := filepath.Join(e.fs.Root(), "files", "fabric_"+uidB, "secret.txt")
	data, err := readFile(onDisk)
	if err != nil || string(data) != "B-only-content" {
		t.Fatalf("B's file damaged: err=%v data=%q", err, data)
	}

	// And A, uploading the same relative path, gets its OWN copy — not B's.
	if resp, _ := e.upload(tokA, "/secret.txt", "A-different", nil); resp.StatusCode != 200 {
		t.Fatalf("A upload: %d", resp.StatusCode)
	}
	adata, _ := readFile(filepath.Join(e.fs.Root(), "files", "fabric_"+uidA, "secret.txt"))
	if string(adata) != "A-different" {
		t.Fatalf("A's copy wrong: %q", adata)
	}
	bdata, _ := readFile(onDisk)
	if string(bdata) != "B-only-content" {
		t.Fatalf("A's write leaked into B's file: %q", bdata)
	}
}

// TestFabricLoginMintsSession: browser SSO — exchange a fabric token for an
// xx-drive session cookie, then use the cookie to reach an authed endpoint.
func TestFabricLoginMintsSession(t *testing.T) {
	e := newFabricEnv(t)
	uid := "99999999999999999999999999999999"
	tok := mintFabric(uid, fabKeyID, fabSecret, fabValidExp)

	body, _ := json.Marshal(map[string]string{"token": tok})
	resp, b := e.req("POST", "/api/auth/fabric", "", bytes.NewReader(body), jsonHdr())
	if resp.StatusCode != 200 {
		t.Fatalf("fabric login: %d %s", resp.StatusCode, b)
	}
	var out struct {
		Token string `json:"token"`
	}
	json.Unmarshal(b, &out)
	if out.Token == "" {
		t.Fatalf("no session token minted")
	}
	// The minted opaque session (contains no dots) authenticates as the same user.
	resp, b = e.req("GET", "/api/auth/me", out.Token, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("me via session: %d", resp.StatusCode)
	}
	var me struct {
		Username string `json:"username"`
		Fabric   bool   `json:"fabric"`
	}
	json.Unmarshal(b, &me)
	if me.Username != "fabric_"+uid || !me.Fabric {
		t.Fatalf("session not bound to fabric identity: %s", b)
	}
}

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }
