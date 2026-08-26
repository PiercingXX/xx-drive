package main

// Integration tests for the CLI verbs and If-Match integration. Reuses the
// sync_test.go harness: a REAL server stack (internal/api + internal/store +
// internal/fsdrv) behind an httptest server, with the CLI config pointed at it.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xxdrive/internal/fsdrv"
)

// captureStdout redirects os.Stdout while fn runs and returns what printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runCmd invokes a cmd* function with args, failing the test on error, and
// returns its stdout.
func runCmd(t *testing.T, fn func([]string) error, args ...string) string {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() {
		runErr = fn(args)
	})
	if runErr != nil {
		t.Fatalf("command %v failed: %v", args, runErr)
	}
	return out
}

// ---- whoami / logout / sessions ----

func TestWhoamiReturnsLoggedInUser(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)

	out := runCmd(t, cmdWhoami)
	if !strings.Contains(out, testUser) {
		t.Fatalf("whoami output %q missing user %q", out, testUser)
	}
	if strings.Contains(out, "[admin]") {
		t.Fatalf("non-admin user reported as admin: %q", out)
	}
}

func TestLogoutClearsStoredTokenAndKillsSession(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)

	runCmd(t, cmdLogout)

	c, err := loadCfg()
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "" {
		t.Fatalf("token still stored after logout: %q", c.Token)
	}
	// The old token must be dead server-side too.
	if _, err := doJSON("GET", env.baseURL+"/api/auth/me", env.token, nil); err == nil {
		t.Fatal("old token still accepted after logout")
	}
}

func TestSessionsListsCurrentAndRevokesOthers(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)

	out := runCmd(t, cmdSessions)
	lines := nonEmptyLines(out)
	if len(lines) < 1 {
		t.Fatalf("expected at least one session, got %q", out)
	}
	foundCurrent := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "*") {
			foundCurrent = true
		}
	}
	if !foundCurrent {
		t.Fatalf("no session marked current in %q", out)
	}

	// A second login (another "machine") gives revoke-others something to do.
	second, err := doJSON("POST", env.baseURL+"/api/auth/login", "", map[string]string{
		"username": testUser, "password": testPass,
	})
	if err != nil {
		t.Fatal(err)
	}
	tok2, _ := second["token"].(string)

	out = runCmd(t, cmdSessions, "revoke-others")
	if !strings.Contains(out, "revoked 1") {
		t.Fatalf("revoke-others output %q, want revoked 1", out)
	}
	if _, err := doJSON("GET", env.baseURL+"/api/auth/me", tok2, nil); err == nil {
		t.Fatal("other session survived revoke-others")
	}
	// Current session still works.
	if out := runCmd(t, cmdWhoami); !strings.Contains(out, testUser) {
		t.Fatalf("current session broken after revoke-others: %q", out)
	}
}

// ---- trash ----

func TestTrashRoundtripRmListRestoreDeleteEmpty(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)

	env.putRemote(t, "/tr/doc.txt", "trash me")
	if err := cmdRm([]string{"/tr/doc.txt"}); err != nil {
		t.Fatalf("rm: %v", err)
	}

	out := runCmd(t, cmdTrash, "ls")
	id := ""
	for _, ln := range nonEmptyLines(out) {
		if strings.Contains(ln, "/tr/doc.txt") {
			id = strings.Fields(ln)[0]
		}
	}
	if id == "" {
		t.Fatalf("trash ls does not show /tr/doc.txt: %q", out)
	}

	runCmd(t, cmdTrash, "restore", id)
	if got := env.fetchRemote(t, "/tr/doc.txt"); got != "trash me" {
		t.Fatalf("restored content wrong: %q", got)
	}

	// Purge one item permanently.
	env.deleteRemote(t, "/tr/doc.txt")
	out = runCmd(t, cmdTrash, "ls")
	id = ""
	for _, ln := range nonEmptyLines(out) {
		if strings.Contains(ln, "/tr/doc.txt") {
			id = strings.Fields(ln)[0]
		}
	}
	if id == "" {
		t.Fatalf("item missing from trash before delete: %q", out)
	}
	runCmd(t, cmdTrash, "delete", id)
	if out := runCmd(t, cmdTrash, "ls"); strings.Contains(out, "/tr/doc.txt") {
		t.Fatalf("purged item still listed: %q", out)
	}

	// Empty whatever remains.
	env.putRemote(t, "/tr/other.txt", "also gone")
	env.deleteRemote(t, "/tr/other.txt")
	out = runCmd(t, cmdTrash, "empty")
	if !strings.Contains(out, "trash emptied (1 items)") {
		t.Fatalf("empty output %q", out)
	}
	if out := runCmd(t, cmdTrash, "ls"); strings.TrimSpace(out) != "" {
		t.Fatalf("trash not empty: %q", out)
	}
}

// ---- versions ----

func TestVersionsOverwriteListGetRestore(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)

	env.putRemote(t, "/vr/doc.txt", "version one")
	local := filepath.Join(t.TempDir(), "doc.txt")
	writeLocal(t, local, "version two")
	if err := cmdUp([]string{local, "/vr/doc.txt"}); err != nil {
		t.Fatalf("overwrite up: %v", err)
	}

	out := runCmd(t, cmdVersions, "ls", "/vr/doc.txt")
	lines := nonEmptyLines(out)
	if len(lines) == 0 {
		t.Fatalf("versions ls empty after overwrite: %q", out)
	}
	vid := strings.Fields(lines[0])[0]

	// Download that version to a file AND to stdout; both carry v1 bytes.
	dst := filepath.Join(t.TempDir(), "v1.bin")
	runCmd(t, cmdVersions, "get", "/vr/doc.txt", vid, dst)
	if got := readLocal(t, dst); got != "version one" {
		t.Fatalf("version download content wrong: %q", got)
	}
	if got := runCmd(t, cmdVersions, "get", "/vr/doc.txt", vid); got != "version one" {
		t.Fatalf("stdout version stream wrong: %q", got)
	}

	// Restore rolls the file back (the restore itself snapshots "version two").
	runCmd(t, cmdVersions, "restore", "/vr/doc.txt", vid)
	if got := env.fetchRemote(t, "/vr/doc.txt"); got != "version one" {
		t.Fatalf("restore did not revert content: %q", got)
	}
}

// ---- shares ----

func TestShareCreateURLRevokeLifecycle(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)

	env.putRemote(t, "/sh/pub.txt", "public bytes")

	out := runCmd(t, cmdShare, "create", "/sh/pub.txt")
	i := strings.Index(out, "/s/")
	if i < 0 {
		t.Fatalf("create output lacks share URL: %q", out)
	}
	token := strings.TrimSpace(strings.TrimPrefix(out[i:], "/s/"))
	if token == "" {
		t.Fatalf("empty share token parsed from %q", out)
	}

	// `share ls` shows the share with its short URL.
	lsOut := runCmd(t, cmdShare, "ls")
	if !strings.Contains(lsOut, "/sh/pub.txt") || !strings.Contains(lsOut, "/s/") {
		t.Fatalf("share ls output incomplete: %q", lsOut)
	}

	// Anonymous access works via the raw capability token.
	body, status := publicFetch(t, env.baseURL+"/s/"+token+"/download?path=/sh/pub.txt")
	if status != 200 || body != "public bytes" {
		t.Fatalf("public download before revoke: status=%d body=%q", status, body)
	}

	// Revoke accepts the RAW token (>16 chars → hashed to the revoke id).
	runCmd(t, cmdShare, "revoke", token)
	body, status = publicFetch(t, env.baseURL+"/s/"+token+"/download?path=/sh/pub.txt")
	if status == 200 {
		t.Fatalf("public download still succeeds after revoke: %q", body)
	}
	if out := runCmd(t, cmdShare, "ls"); strings.Contains(out, "/sh/pub.txt") {
		t.Fatalf("revoked share still listed: %q", out)
	}
}

func publicFetch(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// ---- search / star ----

func TestSearchFindsUploadedFile(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)

	env.putRemote(t, "/se/q4-report-final.txt", "numbers")

	out := runCmd(t, cmdSearch, "q4-report-final")
	if !strings.Contains(out, "/se/q4-report-final.txt") {
		t.Fatalf("search missed the uploaded file: %q", out)
	}
}

func TestStarToggleAndStarredList(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)

	env.putRemote(t, "/st/keep.txt", "x")

	out := runCmd(t, cmdStarToggle, "/st/keep.txt")
	if !strings.HasPrefix(out, "starred ") {
		t.Fatalf("first toggle should star: %q", out)
	}
	if lst := runCmd(t, cmdStarred); !strings.Contains(lst, "/st/keep.txt") {
		t.Fatalf("starred list missing entry: %q", lst)
	}

	out = runCmd(t, cmdStarToggle, "/st/keep.txt")
	if !strings.HasPrefix(out, "unstarred ") {
		t.Fatalf("second toggle should unstar: %q", out)
	}
	if lst := runCmd(t, cmdStarred); strings.Contains(lst, "/st/keep.txt") {
		t.Fatalf("starred list still shows unstared path: %q", lst)
	}
}

// ---- If-Match on upload ----

func TestUpIfMatchRejectsStaleEtagWithoutClobber(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)

	env.putRemote(t, "/im/guarded.txt", "server original")
	ents, err := listDir(env.cfg(), "/im")
	if err != nil || len(ents) != 1 {
		t.Fatalf("list /im: %v (%d entries)", err, len(ents))
	}
	currentEtag := fsdrv.EtagOf(ents[0].Mtime, ents[0].Size)

	local := filepath.Join(t.TempDir(), "guarded.txt")
	writeLocal(t, local, "client clobber attempt")

	// Stale etag → 412 surfaces as an error, remote keeps its bytes,
	// no version snapshot is created from the rejected payload.
	err = cmdUp([]string{"--if-match", `"deadbeef-1"`, local, "/im/guarded.txt"})
	if err == nil || !strings.Contains(err.Error(), "412") {
		t.Fatalf("stale If-Match must fail with 412, got %v", err)
	}
	if got := env.fetchRemote(t, "/im/guarded.txt"); got != "server original" {
		t.Fatalf("remote clobbered despite etag guard: %q", got)
	}

	// Matching etag → overwrite proceeds.
	if err := cmdUp([]string{"--if-match", currentEtag, local, "/im/guarded.txt"}); err != nil {
		t.Fatalf("matching If-Match upload failed: %v", err)
	}
	if got := env.fetchRemote(t, "/im/guarded.txt"); got != "client clobber attempt" {
		t.Fatalf("matching If-Match upload did not land: %q", got)
	}
}

func TestSyncStaleEtagPushBecomesConflictNotClobber(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)
	local := t.TempDir()

	writeLocal(t, filepath.Join(local, "a.txt"), "orig")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	// Forge a STALE etag into the baseline: size/mtime stay truthful (so the
	// listing classifies the change as local-only), but the recorded etag no
	// longer matches the server — simulating another machine's commit landing
	// between listing and push. The guarded push must be rejected with 412
	// and routed into the conflict path, never silently clobbering.
	bp := basePathFor(local, "/sync")
	raw, err := os.ReadFile(bp)
	if err != nil {
		t.Fatal(err)
	}
	var base map[string]syncMeta
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	e, ok := base["/a.txt"]
	if !ok || e.Etag == "" {
		t.Fatalf("baseline entry missing or has no etag: %v", base["/a.txt"])
	}
	e.Etag = `"dead00beef-1"`
	base["/a.txt"] = e
	if raw, err = json.Marshal(base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bp, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	writeLocal(t, filepath.Join(local, "a.txt"), "local edit")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("conflict sync: %v", err)
	}

	// Remote kept the original bytes — no silent clobber.
	if got := env.fetchRemote(t, "/sync/a.txt"); got != "orig" {
		t.Fatalf("remote clobbered by stale-etag push: %q", got)
	}
	// Canonical local converged on the remote version...
	if got := readLocal(t, filepath.Join(local, "a.txt")); got != "orig" {
		t.Fatalf("canonical local not converged: %q", got)
	}
	// ...and the local edit survives as a conflict copy on both sides.
	relCopy := findByName(t, local, "(conflict from")
	if relCopy == "" {
		t.Fatal("no local conflict copy parked for the stale-etag edit")
	}
	if got := readLocal(t, filepath.Join(local, relCopy)); got != "local edit" {
		t.Fatalf("conflict copy content wrong: %q", got)
	}
	remoteEnts, err := walkRemote(env.cfg(), "/sync")
	if err != nil {
		t.Fatal(err)
	}
	remoteCopy := ""
	for p := range remoteEnts {
		if strings.Contains(filepath.Base(p), "(conflict from") {
			remoteCopy = p
		}
	}
	if remoteCopy == "" {
		t.Fatal("no remote conflict copy found")
	}
	if got := env.fetchRemote(t, remoteCopy); got != "local edit" {
		t.Fatalf("remote conflict copy content wrong: %q", got)
	}

	// Next pass is a clean no-op on both sides.
	beforeL := localFiles(t, local)
	beforeR, _ := walkRemote(env.cfg(), "/sync")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("resync: %v", err)
	}
	afterL := localFiles(t, local)
	if len(beforeL) != len(afterL) {
		t.Fatalf("resync mutated local tree: before=%v after=%v", beforeL, afterL)
	}
	for p, c := range beforeL {
		if got := afterL[p]; got != c {
			t.Fatalf("resync changed local %s: %q -> %q", p, c, got)
		}
	}
	afterR, _ := walkRemote(env.cfg(), "/sync")
	if len(beforeR) != len(afterR) {
		t.Fatalf("resync mutated remote tree: before=%v after=%v", beforeR, afterR)
	}
}

// nonEmptyLines splits captured output into non-blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}
