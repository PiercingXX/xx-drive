package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xxdrive/internal/fsdrv"
	"xxdrive/internal/store"
)

type testEnv struct {
	t       *testing.T
	st      *store.Store
	fs      *fsdrv.Driver
	srv     *httptest.Server
	client  *http.Client
	baseURL string
}

func newTestEnv(t *testing.T) *testEnv {
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
	cfg := Config{Addr: ":0", MaxUploadMB: 8, SessionTTL: time.Hour, TrashRetentionDays: 30}
	s := New(cfg, st, fsd, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	return &testEnv{t: t, st: st, fs: fsd, srv: ts, client: ts.Client(), baseURL: ts.URL}
}

func (e *testEnv) req(method, path, token string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.baseURL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func jsonHdr() map[string]string { return map[string]string{"Content-Type": "application/json"} }

func (e *testEnv) login(username, password string) string {
	e.t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, b := e.req("POST", "/api/auth/login", "", bytes.NewReader(body), jsonHdr())
	if resp.StatusCode != 200 {
		e.t.Fatalf("login %s: %d %s", username, resp.StatusCode, b)
	}
	var out struct {
		Token string
	}
	json.Unmarshal(b, &out)
	return out.Token
}

func (e *testEnv) mkUser(name string) string {
	e.t.Helper()
	if _, err := e.st.CreateUser(name, store.HashPassword("password123"), false); err != nil {
		e.t.Fatal(err)
	}
	return e.login(name, "password123")
}

func (e *testEnv) upload(token, path, content string, hdr map[string]string) (*http.Response, map[string]any) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filepath.Base(path))
	fw.Write([]byte(content))
	mw.Close()
	hdrs := map[string]string{"Content-Type": mw.FormDataContentType()}
	for k, v := range hdr {
		hdrs[k] = v
	}
	resp, b := e.req("POST", "/api/files/upload?path="+url.QueryEscape(path), token, &buf, hdrs)
	var out map[string]any
	json.Unmarshal(b, &out)
	return resp, out
}

func postJSON(e *testEnv, token, path string, payload any) (*http.Response, []byte) {
	body, _ := json.Marshal(payload)
	return e.req("POST", path, token, bytes.NewReader(body), jsonHdr())
}

// ---- tests ----

func TestAuthFlow(t *testing.T) {
	env := newTestEnv(t)

	body, _ := json.Marshal(map[string]string{"username": "nobody", "password": "wrong"})
	resp, _ := env.req("POST", "/api/auth/login", "", bytes.NewReader(body), jsonHdr())
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	tok := env.mkUser("alice")

	resp, b := env.req("GET", "/api/auth/me", tok, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "alice") {
		t.Fatalf("me: %d %s", resp.StatusCode, b)
	}

	resp, _ = env.req("GET", "/api/files/list?path=/", "", nil, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 for anon, got %d", resp.StatusCode)
	}

	env.req("POST", "/api/auth/logout", tok, nil, nil)
	resp, _ = env.req("GET", "/api/auth/me", tok, nil, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("session should be dead after logout, got %d", resp.StatusCode)
	}
}

func TestCSRFOriginGuard(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")

	// cookie-authenticated mutating request without Origin must be rejected
	req, _ := http.NewRequest("POST", env.baseURL+"/api/files/mkdir", strings.NewReader(`{"path":"/x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie mutation without Origin should be 403, got %d", resp.StatusCode)
	}

	// bearer-authenticated same request is fine (CLI clients have no ambient creds)
	resp, _ = postJSON(env, tok, "/api/files/mkdir", map[string]string{"path": "/ok"})
	if resp.StatusCode != 200 {
		t.Fatalf("bearer mutation should pass, got %d", resp.StatusCode)
	}
}

func TestFilesEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")

	resp, _ := postJSON(env, tok, "/api/files/mkdir", map[string]string{"path": "/docs"})
	if resp.StatusCode != 200 {
		t.Fatalf("mkdir: %d", resp.StatusCode)
	}

	resp, out := env.upload(tok, "/docs/hello.txt", "hello world", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %v", resp.StatusCode, out)
	}

	resp, b := env.req("GET", "/api/files/list?path=/docs", tok, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "hello.txt") {
		t.Fatalf("list: %d %s", resp.StatusCode, b)
	}

	resp, b = env.req("GET", "/api/files/download?path=/docs/hello.txt", tok, nil, nil)
	if resp.StatusCode != 200 || string(b) != "hello world" {
		t.Fatalf("download: %d %q", resp.StatusCode, b)
	}

	// range request
	req2, _ := http.NewRequest("GET", env.baseURL+"/api/files/download?path=/docs/hello.txt", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("Range", "bytes=0-4")
	r2, _ := env.client.Do(req2)
	rb, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 206 || string(rb) != "hello" {
		t.Fatalf("range: %d %q", r2.StatusCode, rb)
	}

	// overwrite → old version snapshotted and listed
	resp, _ = env.upload(tok, "/docs/hello.txt", "v2 content", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("overwrite: %d", resp.StatusCode)
	}
	resp, b = env.req("GET", "/api/versions?path=/docs/hello.txt", tok, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "versionId") {
		t.Fatalf("versions after overwrite: %d %s", resp.StatusCode, b)
	}

	// If-Match mismatch → 412, content unchanged
	resp, _ = env.upload(tok, "/docs/hello.txt", "clobber!", map[string]string{"If-Match": `"deadbeef-1"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("if-match mismatch should be 412, got %d", resp.StatusCode)
	}
	resp, b = env.req("GET", "/api/files/download?path=/docs/hello.txt", tok, nil, nil)
	if string(b) != "v2 content" {
		t.Fatalf("content changed despite etag guard: %q", b)
	}

	// traversal attempt never leaks
	resp, _ = env.req("GET", "/api/files/download?path=../../etc/passwd", tok, nil, nil)
	if resp.StatusCode == 200 {
		t.Fatal("traversal download must not succeed")
	}

	// delete → trash → restore round trip
	resp, _ = postJSON(env, tok, "/api/files/delete", map[string]string{"path": "/docs/hello.txt"})
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp, b = env.req("GET", "/api/trash", tok, nil, nil)
	if !strings.Contains(string(b), "hello.txt") {
		t.Fatalf("trash list missing item: %s", b)
	}
	var trash []struct {
		ID string `json:"id"`
	}
	json.Unmarshal(b, &trash)
	resp, _ = postJSON(env, tok, "/api/trash/restore", map[string]string{"id": trash[0].ID})
	if resp.StatusCode != 200 {
		t.Fatalf("restore: %d", resp.StatusCode)
	}
	resp, b = env.req("GET", "/api/files/download?path=/docs/hello.txt", tok, nil, nil)
	if resp.StatusCode != 200 || string(b) != "v2 content" {
		t.Fatalf("restored content wrong: %d %q", resp.StatusCode, b)
	}
}

func TestShareFlow(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	env.upload(tok, "/shared.bin", "share me", nil)

	body, _ := json.Marshal(map[string]any{"path": "/shared.bin", "password": "linkpw", "expiresInDays": 7})
	resp, b := env.req("POST", "/api/shares", tok, bytes.NewReader(body), jsonHdr())
	if resp.StatusCode != 200 {
		t.Fatalf("share create: %d %s", resp.StatusCode, b)
	}
	var sh struct {
		Token string
	}
	json.Unmarshal(b, &sh)
	if sh.Token == "" {
		t.Fatal("empty share token")
	}

	// anonymous download without password → 401 (full capability-token URL)
	resp, _ = env.req("GET", "/s/"+sh.Token+"/download?path=/shared.bin", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon without pw should be 401, got %d", resp.StatusCode)
	}
	// short hash-prefix URL must resolve to the same share
	resp, _ = env.req("GET", "/api/shares", tok, nil, nil)
	var listed []struct{ TokenHash string }
	json.Unmarshal(b, &listed) // b currently holds create-response; refresh below
	resp, lb := env.req("GET", "/api/shares", tok, nil, nil)
	json.Unmarshal(lb, &listed)
	if len(listed) != 1 {
		t.Fatalf("expected 1 listed share, got %d", len(listed))
	}
	resp, _ = env.req("GET", "/s/"+listed[0].TokenHash[:16]+"/download?path=/shared.bin", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("short-URL anon should also be 401, got %d", resp.StatusCode)
	}

	// wrong password → page renders error, no grant cookie
	resp, b = env.req("POST", "/s/"+sh.Token, "", strings.NewReader("password=nope"), jsonHdr())
	if resp.StatusCode == 500 || strings.Contains(string(b), "share me") {
		t.Fatalf("wrong password leaked content")
	}

	// correct password → grant cookie issued (do NOT follow the redirect;
	// the cookie rides on the 303 response itself)
	req, _ := http.NewRequest("POST", env.baseURL+"/s/"+sh.Token, strings.NewReader("password=linkpw"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	nofollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	presp, err := nofollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, presp.Body)
	presp.Body.Close()
	if presp.StatusCode != http.StatusSeeOther {
		t.Fatalf("password submit: %d", presp.StatusCode)
	}
	var grant *http.Cookie
	for _, c := range presp.Cookies() {
		if strings.HasPrefix(c.Name, "xxd_pub_") {
			grant = c
		}
	}
	if grant == nil {
		t.Fatal("no grant cookie issued")
	}

	// now anonymous download works with the grant
	req, _ = http.NewRequest("GET", env.baseURL+"/s/"+sh.Token+"/download?path=/shared.bin", nil)
	req.AddCookie(grant)
	dresp, _ := env.client.Do(req)
	db, _ := io.ReadAll(dresp.Body)
	dresp.Body.Close()
	if dresp.StatusCode != 200 || string(db) != "share me" {
		t.Fatalf("granted download: %d %q", dresp.StatusCode, db)
	}

	// revoke → access dies even with grant
	resp, lb = env.req("GET", "/api/shares", tok, nil, nil)
	var shares []struct {
		TokenHash string
		Path      string
	}
	json.Unmarshal(lb, &shares)
	if len(shares) != 1 {
		t.Fatalf("expected 1 share, got %d (%s)", len(shares), lb)
	}
	resp, _ = env.req("DELETE", "/api/shares/"+shares[0].TokenHash, tok, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", env.baseURL+"/s/"+sh.Token+"/download?path=/shared.bin", nil)
	req.AddCookie(grant)
	dresp, _ = env.client.Do(req)
	io.Copy(io.Discard, dresp.Body)
	dresp.Body.Close()
	if dresp.StatusCode != 404 {
		t.Fatalf("revoked share should 404, got %d", dresp.StatusCode)
	}
}

func TestAdminAndIsolation(t *testing.T) {
	env := newTestEnv(t)

	// bootstrap admin exists? create directly for test determinism
	if _, err := env.st.CreateUser("root", store.HashPassword("adminpass123"), true); err != nil {
		t.Fatal(err)
	}
	adminTok := env.login("root", "adminpass123")
	userTok := env.mkUser("alice")

	// non-admin blocked from admin API
	resp, _ := env.req("GET", "/api/admin/users", userTok, nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin should be 403, got %d", resp.StatusCode)
	}

	// admin creates user
	resp, _ = postJSON(env, adminTok, "/api/admin/users", map[string]any{"username": "bob", "password": "bobpass123", "isAdmin": false})
	if resp.StatusCode != 200 {
		t.Fatalf("create user: %d", resp.StatusCode)
	}

	// isolation: alice cannot see bob's files
	bobTok := env.login("bob", "bobpass123")
	env.upload(bobTok, "/bobs-secret.txt", "bob data", nil)
	resp, b := env.req("GET", "/api/files/list?path=/", userTok, nil, nil)
	if strings.Contains(string(b), "bobs-secret") {
		t.Fatal("user isolation broken: alice sees bob's file")
	}
	resp, _ = env.req("GET", "/api/files/download?path=/bobs-secret.txt", userTok, nil, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("alice downloading bob's file: %d", resp.StatusCode)
	}

	// disable bob → sessions die
	resp, _ = postJSON(env, adminTok, "/api/admin/users/set-state", map[string]any{"username": "bob", "disabled": true})
	if resp.StatusCode != 200 {
		t.Fatalf("disable: %d", resp.StatusCode)
	}
	resp, _ = env.req("GET", "/api/auth/me", bobTok, nil, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("disabled user session should die, got %d", resp.StatusCode)
	}
}

func TestSearchStarsEventsZip(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	env.upload(tok, "/pics/sunset photo.jpg", "IMG", nil)
	env.upload(tok, "/readme.md", "# hi", nil)

	resp, b := env.req("GET", "/api/search?q=sunset", tok, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "sunset photo.jpg") {
		t.Fatalf("search: %d %s", resp.StatusCode, b)
	}

	resp, _ = postJSON(env, tok, "/api/star/toggle", map[string]string{"path": "/readme.md"})
	resp, b = env.req("GET", "/api/starred", tok, nil, nil)
	if !strings.Contains(string(b), "readme.md") {
		t.Fatalf("starred: %s", b)
	}

	resp, b = env.req("GET", "/api/events?limit=10", tok, nil, nil)
	if !strings.Contains(string(b), "upload") {
		t.Fatalf("events: %s", b)
	}

	// zip of /pics
	resp, b = env.req("GET", "/api/files/zip?path=/pics", tok, nil, nil)
	if resp.StatusCode != 200 || len(b) < 4 || string(b[:2]) != "PK" {
		t.Fatalf("zip: %d len=%d magic=%q", resp.StatusCode, len(b), b[:min(2, len(b))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestPWAAssetsServed(t *testing.T) {
	env := newTestEnv(t)
	resp, b := env.req("GET", "/", "", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "xx-drive") {
		t.Fatalf("index: %d %s", resp.StatusCode, b[:min(80, len(b))])
	}
	resp, _ = env.req("GET", "/manifest.webmanifest", "", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manifest: %d", resp.StatusCode)
	}
	resp, _ = env.req("GET", "/sw.js", "", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("sw.js: %d", resp.StatusCode)
	}
	resp, _ = env.req("GET", "/healthz", "", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}

var _ = os.Getenv // keep os import if unused by future edits
