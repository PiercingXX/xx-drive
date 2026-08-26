package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xxdrive/internal/store"
)

func mkShare(t *testing.T, e *testEnv, tok, path, pw string, allowDl bool) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"path": path, "password": pw, "allowDownload": allowDl})
	resp, b := e.req("POST", "/api/shares", tok, bytes.NewReader(body), jsonHdr())
	if resp.StatusCode != 200 {
		e.t.Fatalf("share create %s: %d %s", path, resp.StatusCode, b)
	}
	var out struct{ Token string }
	json.Unmarshal(b, &out)
	if out.Token == "" {
		t.Fatal("empty share token")
	}
	return out.Token
}

// submitPassword POSTs the unlock form without following the redirect and
// returns every Set-Cookie from the response.
func submitPassword(t *testing.T, e *testEnv, token, sub, pw string) (*http.Response, []*http.Cookie) {
	t.Helper()
	form := "password=" + pw
	if sub != "" {
		form += "&sub=" + sub
	}
	req, _ := http.NewRequest("POST", e.baseURL+"/s/"+token, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	nofollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := nofollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, resp.Cookies()
}

// ---- item 1: password-protected pages render an unlock form ----

func TestSharePasswordFormRenders(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	env.upload(tok, "/secret.bin", "TOPSECRET", nil)
	token := mkShare(t, env, tok, "/secret.bin", "linkpw123", true)

	// locked page: form present, content and Download hidden
	resp, b := env.req("GET", "/s/"+token, "", nil, nil)
	html := string(b)
	if resp.StatusCode != 200 {
		t.Fatalf("locked page: %d", resp.StatusCode)
	}
	for _, want := range []string{`<form`, `method="post"`, `name="password"`, `type="submit"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("password form missing %q in: %s", want, html)
		}
	}
	if strings.Contains(html, "TOPSECRET") {
		t.Fatal("file content leaked before unlock")
	}
	if strings.Contains(html, ">Download<") || strings.Contains(html, "/download?") {
		t.Fatal("download offered before grant exists")
	}

	// wrong password re-renders the form with the error, still locked
	resp, b = env.req("POST", "/s/"+token, "", strings.NewReader("password=nope"),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	html = string(b)
	if resp.StatusCode != 200 || !strings.Contains(html, "Wrong password") || !strings.Contains(html, `name="password"`) {
		t.Fatalf("wrong-password page wrong: %d %s", resp.StatusCode, html)
	}
	if strings.Contains(html, "TOPSECRET") {
		t.Fatal("content leaked after wrong password")
	}

	// correct password unlocks; unlocked page has Download and no form
	presp, cookies := submitPassword(t, env, token, "", "linkpw123")
	if presp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unlock submit: %d", presp.StatusCode)
	}
	var grant *http.Cookie
	for _, c := range cookies {
		if strings.HasPrefix(c.Name, "xxd_pub_") {
			grant = c
		}
	}
	if grant == nil {
		t.Fatal("no grant cookie issued")
	}
	req, _ := http.NewRequest("GET", env.baseURL+"/s/"+token, nil)
	req.AddCookie(grant)
	gresp, gerr := env.client.Do(req)
	if gerr != nil {
		t.Fatal(gerr)
	}
	gb, _ := io.ReadAll(gresp.Body)
	gresp.Body.Close()
	ghtml := string(gb)
	if gresp.StatusCode != 200 || !strings.Contains(ghtml, ">Download<") {
		t.Fatalf("unlocked page missing download: %d %s", gresp.StatusCode, ghtml)
	}
	if strings.Contains(ghtml, "<form") {
		t.Fatal("unlocked page should not show the password form")
	}
}

// ---- item 1b: password guesses are throttled (10 fails / 15 min → 429) ----

func TestSharePasswordThrottled(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	env.upload(tok, "/locked.bin", "LOCKED", nil)
	token := mkShare(t, env, tok, "/locked.bin", "linkpw123", true)

	// 10 wrong guesses burn the bucket; the 11th attempt is refused with 429
	// BEFORE any PBKDF2 verification runs.
	last := 0
	for i := 0; i < 11; i++ {
		resp, b := env.req("POST", "/s/"+token, "", strings.NewReader("password=nope"),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		last = resp.StatusCode
		if i < 10 && last != 200 {
			t.Fatalf("guess %d: got %d, want 200 (wrong-password page): %s", i+1, last, b)
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("11th wrong-password submit: got %d, want 429", last)
	}

	// a CORRECT password no longer unlocks either — the bucket applies to all
	// attempts from that client for the share
	presp, cookies := submitPassword(t, env, token, "", "linkpw123")
	if presp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("correct password while limited: got %d, want 429", presp.StatusCode)
	}
	for _, c := range cookies {
		if strings.HasPrefix(c.Name, "xxd_pub_") {
			t.Fatal("grant cookie issued while rate-limited")
		}
	}
}

// ---- item 8: wrong-password re-render preserves ?sub= context ----

func TestSharePasswordErrorKeepsSub(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	mustUpload(t, env, tok, "/pics/inner.txt", "inner-content")
	token := mkShare(t, env, tok, "/pics", "folderpw1", true)

	form := "password=nope&sub=%2Finner.txt"
	resp, b := env.req("POST", "/s/"+token, "", strings.NewReader(form),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	html := string(b)
	if resp.StatusCode != 200 || !strings.Contains(html, "Wrong password") {
		t.Fatalf("wrong-password page: %d", resp.StatusCode)
	}
	if !strings.Contains(html, `name="sub" value="/inner.txt"`) {
		t.Fatalf("error re-render dropped sub context: %s", html)
	}
}

// ---- item 2: folder shares list children, download children, reject escapes ----

func TestFolderShareBrowseDownloadContainment(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	mustUpload(t, env, tok, "/pics/one.txt", "one-content")
	mustUpload(t, env, tok, "/pics/sub/deep.txt", "deep-content")
	env.upload(tok, "/topsecret.txt", "outside-share", nil)

	token := mkShare(t, env, tok, "/pics", "", true)

	// page lists children with page links relative to the share root
	resp, b := env.req("GET", "/s/"+token, "", nil, nil)
	html := string(b)
	if resp.StatusCode != 200 {
		t.Fatalf("folder page: %d", resp.StatusCode)
	}
	if !strings.Contains(html, "one.txt") {
		t.Fatalf("child file missing from listing: %s", html)
	}
	if !strings.Contains(html, `href="/s/`+token+`?sub=%2Fone.txt"`) {
		t.Fatalf("file entry must link to the page with share-relative sub: %s", html)
	}
	if strings.Contains(html, "?path=") {
		t.Fatalf("HTML entries must not emit ?path= full logical paths: %s", html)
	}

	// list a child directory via sub (relative) AND via path (full logical)
	resp, b = env.req("GET", "/s/"+token+"/list?sub=/sub", "", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "deep.txt") {
		t.Fatalf("list child dir via sub: %d %s", resp.StatusCode, b)
	}
	resp, b = env.req("GET", "/s/"+token+"/list?path=/pics/sub", "", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "deep.txt") {
		t.Fatalf("list child dir via path: %d %s", resp.StatusCode, b)
	}

	// download a child file both ways
	resp, b = env.req("GET", "/s/"+token+"/download?sub=/one.txt", "", nil, nil)
	if resp.StatusCode != 200 || string(b) != "one-content" {
		t.Fatalf("download child via sub: %d %q", resp.StatusCode, b)
	}
	resp, b = env.req("GET", "/s/"+token+"/download?path=/pics/sub/deep.txt", "", nil, nil)
	if resp.StatusCode != 200 || string(b) != "deep-content" {
		t.Fatalf("download child via path: %d %q", resp.StatusCode, b)
	}

	// absolute sub on a nested share must NOT double the prefix (old joinSub bug)
	token2 := mkShare(t, env, tok, "/pics/sub", "", true)
	resp, b = env.req("GET", "/s/"+token2+"/list?sub=/pics/sub", "", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "deep.txt") {
		t.Fatalf("absolute sub doubled prefix: %d %s", resp.StatusCode, b)
	}
	resp, b = env.req("GET", "/s/"+token2+"/download?sub=/deep.txt", "", nil, nil)
	if resp.StatusCode != 200 || string(b) != "deep-content" {
		t.Fatalf("nested share child download: %d %q", resp.StatusCode, b)
	}

	// containment: outside the share is rejected everywhere
	for _, bad := range []string{
		"/s/" + token + "/download?path=/topsecret.txt",
		"/s/" + token + "/download?path=/pics/../../topsecret.txt",
		"/s/" + token + "/download?sub=../../x",
		"/s/" + token + "/list?path=/topsecret.txt",
		"/s/" + token + "/list?sub=../../../etc",
	} {
		resp, _ = env.req("GET", bad, "", nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: want 404 for out-of-share target, got %d", bad, resp.StatusCode)
		}
	}
	// page route also rejects escapes
	resp, _ = env.req("GET", "/s/"+token+"?path=/topsecret.txt", "", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("page escape: want 404, got %d", resp.StatusCode)
	}
}

// ---- item 3: view-only shares cannot be exfiltrated via inline=1 ----

func TestViewOnlyInlineAllowlist(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	mustUpload(t, env, tok, "/media/pic.jpg", "JPEGDATA")
	mustUpload(t, env, tok, "/media/doc.pdf", "PDFDATA")
	mustUpload(t, env, tok, "/media/arc.zip", "ZIPDATA")

	token := mkShare(t, env, tok, "/media", "", false)

	cases := []struct {
		q    string
		want int
	}{
		{"?sub=/pic.jpg&inline=1", 200},
		{"?sub=/pic.webm&inline=1", 404}, // allowlist type but absent → plain 404, never content
		{"?sub=/doc.pdf&inline=1", 403},
		{"?sub=/arc.zip&inline=1", 403},
		{"?sub=/doc.pdf", 403},
		{"", 403}, // whole-folder zip of a view-only share
	}
	for _, tc := range cases {
		resp, b := env.req("GET", "/s/"+token+"/download"+tc.q, "", nil, nil)
		if resp.StatusCode != tc.want {
			t.Fatalf("view-only %q: want %d, got %d (%s)", tc.q, tc.want, resp.StatusCode, b)
		}
		if tc.want == 403 && bytes.Contains(b, []byte("DATA")) {
			t.Fatalf("view-only %q leaked content", tc.q)
		}
	}
	// inline image actually streams
	resp, b := env.req("GET", "/s/"+token+"/download?sub=/pic.jpg&inline=1", "", nil, nil)
	if string(b) != "JPEGDATA" {
		t.Fatalf("allowed preview broken: %q", b)
	}
	_ = resp
}

// ---- item 4: API returns canonical absolute share URLs when BaseURL set ----

func TestShareListAbsoluteURL(t *testing.T) {
	env := newTestEnvWithCfg(t, func(c *Config) { c.BaseURL = "https://drive.example.com" })
	tok := env.mkUser("alice")
	mustUpload(t, env, tok, "/x.txt", "X")
	mkShare(t, env, tok, "/x.txt", "", true)

	_, b := env.req("GET", "/api/shares", tok, nil, nil)
	var rows []struct{ URL string }
	if err := json.Unmarshal(b, &rows); err != nil || len(rows) != 1 {
		t.Fatalf("shares list: %v %s", err, b)
	}
	if !strings.HasPrefix(rows[0].URL, "https://drive.example.com/s/") {
		t.Fatalf("share URL not absolute/canonical: %q", rows[0].URL)
	}
}

// ---- item 5: Secure attribute matrix on session + grant cookies ----

func TestSecureCookieMatrix(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   bool
	}{
		{"plain-http", nil, false},
		{"https-base-url", func(c *Config) { c.BaseURL = "https://drive.example.com" }, true},
		{"tls-cert", func(c *Config) { c.TLSCert = "/tmp/x.pem" }, true},
		{"forced-flag", func(c *Config) { c.SecureCookies = true }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnvWithCfg(t, tc.mutate)
			tok := env.mkUser("alice")

			// session cookie
			body, _ := json.Marshal(map[string]string{"username": "alice", "password": "password123"})
			resp, _ := env.req("POST", "/api/auth/login", "", bytes.NewReader(body), jsonHdr())
			sessSec := false
			for _, c := range resp.Cookies() {
				if c.Name == sessionCookie {
					sessSec = c.Secure
				}
			}
			if sessSec != tc.want {
				t.Fatalf("session cookie Secure=%v, want %v", sessSec, tc.want)
			}

			// grant cookie follows the same rule
			env.upload(tok, "/f.bin", "data", nil)
			token := mkShare(t, env, tok, "/f.bin", "pw1234567", true)
			_, cookies := submitPassword(t, env, token, "", "pw1234567")
			grantSec := false
			found := false
			for _, c := range cookies {
				if strings.HasPrefix(c.Name, "xxd_pub_") {
					found = true
					grantSec = c.Secure
				}
			}
			if !found {
				t.Fatal("no grant cookie issued")
			}
			if grantSec != tc.want {
				t.Fatalf("grant cookie Secure=%v, want %v", grantSec, tc.want)
			}
		})
	}
}

// ---- item 6: grant map cap + TTL sweep ----

func TestGrantCapAndSweep(t *testing.T) {
	env := newTestEnv(t)
	sh := &store.Share{TokenHash: strings.Repeat("ab", 32)}
	rec := httptest.NewRecorder()

	for i := 0; i < pubGrantMaxEnts+64; i++ {
		env.srv.issueGrant(rec, sh)
	}
	env.srv.pubMu.Lock()
	n := 0
	for _, m := range env.srv.pubGr {
		n += len(m)
	}
	env.srv.pubMu.Unlock()
	if n > pubGrantMaxEnts {
		t.Fatalf("grant map exceeds cap: %d > %d", n, pubGrantMaxEnts)
	}
	if n == 0 {
		t.Fatal("cap logic evicted everything")
	}

	// expired entries are swept
	env.srv.pubMu.Lock()
	env.srv.pubGr[sh.TokenHash]["stale-grant"] = 1 // long past expiry
	env.srv.pubMu.Unlock()
	env.srv.SweepShareGrants()
	env.srv.pubMu.Lock()
	_, alive := env.srv.pubGr[sh.TokenHash]["stale-grant"]
	env.srv.pubMu.Unlock()
	if alive {
		t.Fatal("expired grant survived TTL sweep")
	}
}

// ---- item 7: short hash-prefix URLs keep resolving correctly across revoke ----

func TestShortURLResolveAfterRevoke(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	mustUpload(t, env, tok, "/a.txt", "AAA")
	mustUpload(t, env, tok, "/b.txt", "BBB")

	t1 := mkShare(t, env, tok, "/a.txt", "", true)

	// warm the prefix index, then resolve via short URL
	_, lb := env.req("GET", "/api/shares", tok, nil, nil)
	var shares []struct{ TokenHash string }
	json.Unmarshal(lb, &shares)
	short1 := shares[0].TokenHash[:16]
	resp, b := env.req("GET", "/s/"+short1+"/download?path=/a.txt", "", nil, nil)
	if resp.StatusCode != 200 || string(b) != "AAA" {
		t.Fatalf("short URL before revoke: %d %q", resp.StatusCode, b)
	}

	// create a second share (invalidates index), then revoke the FIRST
	mkShare(t, env, tok, "/b.txt", "", true)
	resp, _ = env.req("DELETE", "/api/shares/"+shares[0].TokenHash, tok, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}

	// revoked share must never resolve — even from a stale warm index
	resp, _ = env.req("GET", "/s/"+short1+"/download?path=/a.txt", "", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked share resolved via short URL: got %d", resp.StatusCode)
	}
	resp, _ = env.req("GET", "/s/"+t1+"/download?path=/a.txt", "", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked share resolved via full token: got %d", resp.StatusCode)
	}

	// surviving share still resolves after the rebuild
	_, lb = env.req("GET", "/api/shares", tok, nil, nil)
	shares = nil
	json.Unmarshal(lb, &shares)
	if len(shares) != 1 {
		t.Fatalf("want 1 remaining share, got %d (%s)", len(shares), lb)
	}
	short2 := shares[0].TokenHash[:16]
	resp, b = env.req("GET", "/s/"+short2+"/download?path=/b.txt", "", nil, nil)
	if resp.StatusCode != 200 || string(b) != "BBB" {
		t.Fatalf("surviving short URL: %d %q", resp.StatusCode, b)
	}
}

// ---- item 8: public shared-folder zip skips planted symlinks ----

func TestSharedFolderZipSkipsSymlinks(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	mustUpload(t, env, tok, "/pub/real.txt", "regular-content")

	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("TOP-SECRET-OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	pubDir := filepath.Join(env.fs.Root(), "files", "alice", "pub")
	links := map[string]string{
		"leak.txt":     outside,
		"dangling.txt": filepath.Join(pubDir, "never-existed.bin"),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(pubDir, name)); err != nil {
			t.Skipf("symlinks unavailable on this filesystem: %v", err)
		}
	}

	token := mkShare(t, env, tok, "/pub", "", true)
	resp, b := env.req("GET", "/s/"+token+"/download", "", nil, nil)
	if resp.StatusCode != 200 || len(b) < 4 || string(b[:2]) != "PK" {
		t.Fatalf("shared zip: %d magic=%q", resp.StatusCode, b[:min(2, len(b))])
	}

	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("unreadable zip: %v", err)
	}
	foundReal := false
	for _, f := range zr.File {
		rc, oerr := f.Open()
		if oerr != nil {
			t.Fatalf("open %s in zip: %v", f.Name, oerr)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		if strings.Contains(string(body), "TOP-SECRET-OUTSIDE") {
			t.Fatalf("public zip leaked planted symlink target via %q", f.Name)
		}
		switch strings.TrimPrefix(f.Name, "pub/") {
		case "real.txt":
			foundReal = string(body) == "regular-content"
		case "leak.txt", "dangling.txt":
			t.Fatalf("symlink entry included in public zip: %q", f.Name)
		}
	}
	if !foundReal {
		t.Fatal("regular file missing from public zip — walker over-skipped")
	}
}

func TestSharePrefixResolvesOnFreshServer(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	env.upload(tok, "/note.txt", "hi", nil)
	_ = mkShare(t, env, tok, "/note.txt", "", true)

	_, listed := env.req("GET", "/api/shares", tok, nil, nil)
	var rows []struct {
		TokenHash string `json:"tokenHash"`
	}
	if err := json.Unmarshal(listed, &rows); err != nil || len(rows) != 1 || len(rows[0].TokenHash) != 16 {
		t.Fatalf("share list: %s err=%v", listed, err)
	}
	prefix := rows[0].TokenHash

	// New Server on the same store/fs — equivalent to a process restart.
	s2 := New(Config{MaxUploadMB: 8, SessionTTL: time.Hour, TrashRetentionDays: 30}, env.st, env.fs, nil)
	ts2 := httptest.NewServer(s2.Handler())
	t.Cleanup(ts2.Close)
	resp, err := ts2.Client().Get(ts2.URL + "/s/" + prefix)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("fresh-server prefix lookup: %d %s", resp.StatusCode, body)
	}
}
