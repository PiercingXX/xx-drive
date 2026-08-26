package main

// Integration tests for the two-way sync engine. Each test spins up the REAL
// server stack (internal/api + internal/store + internal/fsdrv) behind an
// httptest server, points the CLI config at it, and drives syncOnce against a
// temp local dir — the same code path as `xxdrive sync`.

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xxdrive/internal/api"
	"xxdrive/internal/fsdrv"
	"xxdrive/internal/store"
)

const (
	testUser = "cliuser"
	testPass = "password123"
)

type srvEnv struct {
	t        *testing.T
	baseURL  string
	token    string
	dataRoot string
	user     string
}

func newSrvEnv(t *testing.T) *srvEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(dir, "data")
	fsd, err := fsdrv.New(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	apiCfg := api.Config{Addr: ":0", MaxUploadMB: 8, SessionTTL: time.Hour, TrashRetentionDays: 30}
	srv := api.New(apiCfg, st, fsd, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})

	if _, err := st.CreateUser(testUser, store.HashPassword(testPass), false); err != nil {
		t.Fatal(err)
	}
	out, err := doJSON("POST", ts.URL+"/api/auth/login", "", map[string]string{
		"username": testUser, "password": testPass,
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := out["token"].(string)
	if tok == "" {
		t.Fatal("login returned no token")
	}
	return &srvEnv{t: t, baseURL: strings.TrimRight(ts.URL, "/"), token: tok, dataRoot: dataRoot, user: testUser}
}

func (e *srvEnv) cfg() *config { return &config{BaseURL: e.baseURL, Token: e.token} }

// installCfg points the CLI's global config at a throwaway file so
// basePathFor/loadBaseline/saveBaseline stay isolated per test.
func (e *srvEnv) installCfg(t *testing.T) {
	t.Helper()
	old := cfgPath
	cfgPath = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { cfgPath = old })
	if err := saveCfg(e.cfg()); err != nil {
		t.Fatal(err)
	}
}

func writeLocal(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readLocal(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *srvEnv) putRemote(t *testing.T, path, content string) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "seed.bin")
	writeLocal(t, tmp, content)
	if _, err := uploadFile(e.cfg(), tmp, path, "", false); err != nil {
		t.Fatalf("seed remote %s: %v", path, err)
	}
}

func (e *srvEnv) fetchRemote(t *testing.T, path string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := downloadFile(e.cfg(), path, dst); err != nil {
		t.Fatalf("download %s: %v", path, err)
	}
	return readLocal(t, dst)
}

func (e *srvEnv) deleteRemote(t *testing.T, path string) {
	t.Helper()
	if _, err := doJSON("POST", e.baseURL+"/api/files/delete", e.token, map[string]string{"path": path}); err != nil {
		t.Fatalf("remote delete %s: %v", path, err)
	}
}

// findByName walks root and returns the first path relative to root whose file
// name contains substr, or "" when none matches.
func findByName(t *testing.T, root, substr string) string {
	t.Helper()
	found := ""
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.Contains(d.Name(), substr) {
			rel, rerr := filepath.Rel(root, p)
			if rerr == nil {
				found = rel
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}

// localFiles snapshots the local tree (excluding trash) as relPath → content.
func localFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".xxdrive-trash" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		b, _ := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	return out
}

// ---- tests ----

func TestSyncLocalOnlyEditSurvivesRemotely(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)
	local := t.TempDir()

	writeLocal(t, filepath.Join(local, "a.txt"), "original")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	// Local-only edit must OVERWRITE the remote (versioned), not conflict-rename.
	writeLocal(t, filepath.Join(local, "a.txt"), "locally edited v2")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("push sync: %v", err)
	}
	if got := env.fetchRemote(t, "/sync/a.txt"); got != "locally edited v2" {
		t.Fatalf("local edit lost on remote: %q", got)
	}

	// Follow-up pass is a clean no-op: no clobber, no stray copies.
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if got := readLocal(t, filepath.Join(local, "a.txt")); got != "locally edited v2" {
		t.Fatalf("local file clobbered on follow-up pass: %q", got)
	}
	if got := env.fetchRemote(t, "/sync/a.txt"); got != "locally edited v2" {
		t.Fatalf("remote changed on follow-up pass: %q", got)
	}
	if extra := findByName(t, local, "(conflict from"); extra != "" {
		t.Fatalf("unexpected local conflict copy: %s", extra)
	}
	if ents, _ := walkRemote(env.cfg(), "/sync"); len(ents) != 1 {
		t.Fatalf("expected exactly 1 remote entry, got %d", len(ents))
	}
}

func TestSyncRemoteOnlyChangePulledDown(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)
	local := t.TempDir()

	writeLocal(t, filepath.Join(local, "doc.md"), "v1")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	env.putRemote(t, "/sync/doc.md", "remote-only v2 much longer")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("pull sync: %v", err)
	}
	if got := readLocal(t, filepath.Join(local, "doc.md")); got != "remote-only v2 much longer" {
		t.Fatalf("remote change not pulled down: %q", got)
	}

	// Second pass stays stable.
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if got := readLocal(t, filepath.Join(local, "doc.md")); got != "remote-only v2 much longer" {
		t.Fatalf("content changed on resync: %q", got)
	}
}

func TestSyncBothChangedKeepsBothVersionsAndConverges(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)
	local := t.TempDir()

	writeLocal(t, filepath.Join(local, "note.txt"), "base")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	writeLocal(t, filepath.Join(local, "note.txt"), "local-version-of-note")
	env.putRemote(t, "/sync/note.txt", "remote-version-of-note")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("conflict sync: %v", err)
	}

	// Canonical path converged on the remote version on BOTH sides.
	if got := env.fetchRemote(t, "/sync/note.txt"); got != "remote-version-of-note" {
		t.Fatalf("canonical remote wrong: %q", got)
	}
	if got := readLocal(t, filepath.Join(local, "note.txt")); got != "remote-version-of-note" {
		t.Fatalf("canonical local wrong: %q", got)
	}

	// The local version survives as a conflict copy locally AND remotely.
	relCopy := findByName(t, local, "(conflict from")
	if relCopy == "" {
		t.Fatal("no local conflict copy found")
	}
	if got := readLocal(t, filepath.Join(local, relCopy)); got != "local-version-of-note" {
		t.Fatalf("local conflict copy content wrong: %q", got)
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
	if got := env.fetchRemote(t, remoteCopy); got != "local-version-of-note" {
		t.Fatalf("remote conflict copy content wrong: %q", got)
	}

	// Second sync is a clean no-op: identical trees both sides.
	beforeL := localFiles(t, local)
	beforeR, _ := walkRemote(env.cfg(), "/sync")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if afterL := localFiles(t, local); !reflect.DeepEqual(beforeL, afterL) {
		t.Fatalf("second sync mutated local tree:\nbefore=%v\nafter=%v", beforeL, afterL)
	}
	if afterR, _ := walkRemote(env.cfg(), "/sync"); !reflect.DeepEqual(beforeR, afterR) {
		t.Fatalf("second sync mutated remote tree:\nbefore=%v\nafter=%v", beforeR, afterR)
	}
}

func TestSyncBothDeletedStaysDeleted(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)
	local := t.TempDir()

	writeLocal(t, filepath.Join(local, "gone.txt"), "bye")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	if err := os.Remove(filepath.Join(local, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	env.deleteRemote(t, "/sync/gone.txt")

	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("sync after both deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(local, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("deleted file reappeared locally")
	}
	if ents, _ := walkRemote(env.cfg(), "/sync"); len(ents) != 0 {
		t.Fatalf("expected empty remote, got %v", ents)
	}

	// Idempotent: another clean pass, no resurrection.
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if _, err := os.Stat(filepath.Join(local, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("file resurrected by second pass")
	}
}

func TestSameSizeMtimeDifferentContentDocumentedBehavior(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)
	local := t.TempDir()
	T := time.Unix(1_700_000_000, 0)

	// Establish baseline through one side so mtimes converge.
	writeLocal(t, filepath.Join(local, "bin.dat"), "0123456789")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	// Diverge BOTH sides but pin identical size (10) and mtime (T).
	writeLocal(t, filepath.Join(local, "bin.dat"), "abcdefghij")
	if err := os.Chtimes(filepath.Join(local, "bin.dat"), T, T); err != nil {
		t.Fatal(err)
	}
	env.putRemote(t, "/sync/bin.dat", "ABCDEFGHIJ")
	serverAbs := filepath.Join(env.dataRoot, "files", env.user, "sync", "bin.dat")
	if err := os.Chtimes(serverAbs, T, T); err != nil {
		t.Fatalf("pin server mtime: %v", err)
	}

	// Documented design behavior: equal size+mtime is treated as "same
	// content" by the metadata three-way merge. No copies are made and both
	// divergent byte streams are left alone; the shared meta enters the
	// baseline. (Content-identity would require hashing every remote file.)
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := readLocal(t, filepath.Join(local, "bin.dat")); got != "abcdefghij" {
		t.Fatalf("local unexpectedly rewritten: %q", got)
	}
	if got := env.fetchRemote(t, "/sync/bin.dat"); got != "ABCDEFGHIJ" {
		t.Fatalf("remote unexpectedly rewritten: %q", got)
	}
	if files := localFiles(t, local); len(files) != 1 {
		t.Fatalf("copies created for equal size+mtime: %v", files)
	}
	if ents, _ := walkRemote(env.cfg(), "/sync"); len(ents) != 1 {
		t.Fatalf("remote copies created for equal size+mtime: %d", len(ents))
	}
}

func TestRemoteDeleteMovesLocalFileToTrash(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)
	local := t.TempDir()

	writeLocal(t, filepath.Join(local, "keepme.txt"), "precious")
	writeLocal(t, filepath.Join(local, "sub", "dir", "nested.txt"), "nested precious")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	env.deleteRemote(t, "/sync/keepme.txt")
	env.deleteRemote(t, "/sync/sub/dir/nested.txt")
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	for _, p := range []string{"keepme.txt", filepath.Join("sub", "dir", "nested.txt")} {
		if _, err := os.Stat(filepath.Join(local, p)); !os.IsNotExist(err) {
			t.Fatalf("%s still in place; expected move to .xxdrive-trash", p)
		}
		trashRel := findByName(t, filepath.Join(local, ".xxdrive-trash"), filepath.Base(p))
		if trashRel == "" {
			t.Fatalf("%s not found in .xxdrive-trash", p)
		}
		want := "precious"
		if p != "keepme.txt" {
			want = "nested precious"
		}
		if got := readLocal(t, filepath.Join(local, ".xxdrive-trash", trashRel)); got != want {
			t.Fatalf("trashed content wrong for %s: %q", p, got)
		}
	}

	// Trash contents never re-enter reconciliation.
	if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
		t.Fatalf("resync: %v", err)
	}
}

func TestEmptyOrCorruptBaselineDoesNotDeleteLocalFiles(t *testing.T) {
	t.Run("corrupt baseline json", func(t *testing.T) {
		env := newSrvEnv(t)
		env.installCfg(t)
		local := t.TempDir()

		writeLocal(t, filepath.Join(local, "only-copy.txt"), "the only copy")
		bp := basePathFor(local, "/sync")
		if err := os.MkdirAll(filepath.Dir(bp), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bp, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}

		// Remote lacks the file; corrupt baseline has no entry. This used to
		// be the recipe for deleting the only copy. It must survive.
		if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if got := readLocal(t, filepath.Join(local, "only-copy.txt")); got != "the only copy" {
			t.Fatalf("local only copy damaged: %q", got)
		}
	})

	t.Run("empty baseline missing remote", func(t *testing.T) {
		env := newSrvEnv(t)
		env.installCfg(t)
		local := t.TempDir()

		writeLocal(t, filepath.Join(local, "survivor.txt"), "survivor bytes")

		if err := syncOnce(env.cfg(), local, "/sync"); err != nil {
			t.Fatalf("first-ever sync: %v", err)
		}
		if got := readLocal(t, filepath.Join(local, "survivor.txt")); got != "survivor bytes" {
			t.Fatalf("local file damaged on first sync: %q", got)
		}
	})
}
