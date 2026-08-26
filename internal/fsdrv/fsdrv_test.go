package fsdrv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestTraversalCorpus throws the classic path-traversal payloads at the resolver.
// Every payload must either be rejected or resolve strictly inside the user root.
func TestTraversalCorpus(t *testing.T) {
	d := newTestDriver(t)
	payloads := []string{
		"../evil",
		"../../etc/passwd",
		"..%2Fetc%2Fpasswd", // literal percent-encoded (double-decode defense)
		"%2e%2e%2fescape",
		"a/../../../escape",
		"/absolute/path",
		"..\\..\\windows",
		"foo\\bar",
		"with\x00nul",
		"./././ok-but-clean",
		"a/../b/../c",
		strings.Repeat("long/", 1200) + "name",
	}
	for _, p := range payloads {
		abs, logical, err := d.ResolveUserPath("alice", p)
		if err != nil {
			continue // rejected: good
		}
		userRoot := filepath.Join(d.Root(), "files", "alice")
		if !strings.HasPrefix(abs, userRoot+string(os.PathSeparator)) && abs != userRoot {
			t.Errorf("payload %q escaped user root: abs=%q logical=%q", p, abs, logical)
		}
		if strings.Contains(logical, "..") {
			t.Errorf("payload %q produced logical path containing ..: %q", p, logical)
		}
		if strings.Contains(logical, "\\") {
			t.Errorf("payload %q kept backslash in logical path: %q", p, logical)
		}
	}
}

// TestSymlinkEscape ensures a symlink planted inside the tree cannot be used
// to reach outside content: resolution through a symlink component is refused.
func TestSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("top secret"), 0o600)

	d := newTestDriver(t)
	victimRoot := filepath.Join(d.Root(), "files", "alice")
	os.MkdirAll(victimRoot, 0o700)
	os.Symlink(outside, filepath.Join(victimRoot, "link"))

	// Listing through the planted symlink must be refused outright.
	ents, err := d.List("alice", "/link")
	if err == nil {
		for _, e := range ents {
			if e.Name == "secret" {
				t.Fatalf("symlink escape: outside file visible through link")
			}
		}
		t.Logf("list through symlink returned no outside entries")
	} else if !errors.Is(err, ErrInvalid) {
		t.Fatalf("unexpected error type: %v", err)
	}

	// Reading a file through the symlink is likewise refused.
	if _, _, _, err := d.OpenFile("alice", "/link/secret"); err == nil {
		t.Fatal("open through symlink must fail")
	}
}

func TestValidateRelBasics(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"docs", "/docs"},
		{"/docs/", "/docs"},
		{"a/b/c", "/a/b/c"},
		{"a//b", "/a/b"},
		{"a/./b", "/a/b"},
		{"a/b/../c", "/a/c"},
		{"../../../../", "/"},
	}
	for _, c := range cases {
		got, err := ValidateRel(c.in)
		if err != nil {
			t.Errorf("ValidateRel(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ValidateRel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"a\\b", "\x00"} {
		if _, err := ValidateRel(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateRel(%q) should reject, got err=%v", bad, err)
		}
	}
}

func TestFileLifecycleAndVersions(t *testing.T) {
	d := newTestDriver(t)
	const user = "alice"

	// upload v1
	sha1, e, _, prev, err := d.Upload(user, "/docs/report.txt", strings.NewReader("hello v1"), 1<<20, "", false, "")
	if err != nil || e == nil || prev.VersionID != "" {
		t.Fatalf("upload v1: err=%v entry=%v prev=%v", err, e, prev)
	}
	if sha1 == "" {
		t.Fatal("empty sha")
	}

	// overwrite → v1 becomes a version
	_, e2, _, prev2, err := d.Upload(user, "/docs/report.txt", strings.NewReader("hello v2 longer"), 1<<20, "", false, "")
	if err != nil {
		t.Fatalf("upload v2: %v", err)
	}
	if prev2.VersionID == "" || prev2.Size != int64(len("hello v1")) {
		t.Fatalf("expected previous version snapshotted, got %+v", prev2)
	}

	// download current
	f, fi, _, err := d.OpenFile(user, "/docs/report.txt")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, fi.Size())
	f.Read(buf)
	f.Close()
	if string(buf) != "hello v2 longer" {
		t.Fatalf("content mismatch: %q", buf)
	}

	// restore old version
	if err := d.RestoreVersion(user, "/docs/report.txt", prev2.VersionID); err != nil {
		t.Fatalf("restore version: %v", err)
	}
	f, fi, _, _ = d.OpenFile(user, "/docs/report.txt")
	buf = make([]byte, fi.Size())
	f.Read(buf)
	f.Close()
	if string(buf) != "hello v1" {
		t.Fatalf("restore mismatch: %q", buf)
	}

	_ = e2
}

func TestTrashCycle(t *testing.T) {
	d := newTestDriver(t)
	const user = "bob"
	d.Mkdir(user, "/proj")
	d.Upload(user, "/proj/a.txt", strings.NewReader("A"), 1<<20, "", false, "")
	d.Upload(user, "/proj/b.txt", strings.NewReader("B"), 1<<20, "", false, "")

	id, orig, err := d.Delete(user, "/proj")
	if err != nil || id == "" || orig != "/proj" {
		t.Fatalf("delete: id=%q orig=%q err=%v", id, orig, err)
	}
	if _, err := d.Stat(user, "/proj"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected gone, got %v", err)
	}
	items, err := d.ListTrash(user)
	if err != nil || len(items) != 1 || items[0].Name != "proj" || !items[0].IsDir {
		t.Fatalf("trash list: %+v err=%v", items, err)
	}
	dest, err := d.Restore(user, id)
	if err != nil || dest != "/proj" {
		t.Fatalf("restore: dest=%q err=%v", dest, err)
	}
	e, err := d.Stat(user, "/proj/a.txt")
	if err != nil || e.Size != 1 {
		t.Fatalf("restored stat: %+v err=%v", e, err)
	}
}

func TestConflictRenameOnUpload(t *testing.T) {
	d := newTestDriver(t)
	const user = "carol"
	d.Upload(user, "/f.txt", strings.NewReader("original"), 1<<20, "", false, "")
	sha, e, conflicted, _, err := d.Upload(user, "/f.txt", strings.NewReader("from phone"), 1<<20, "", true, "pixel-9")
	if err != nil || !conflicted {
		t.Fatalf("conflict upload: conflicted=%v err=%v", conflicted, err)
	}
	if e.Path == "/f.txt" {
		t.Fatal("conflict copy should have a new name")
	}
	if !strings.Contains(e.Name, "conflict from pixel-9") {
		t.Fatalf("conflict name wrong: %q", e.Name)
	}
	orig, _, _, err := readAll(d, user, "/f.txt")
	if err != nil || orig != "original" {
		t.Fatalf("original overwritten! %q %v", orig, err)
	}
	copyContent, _, _, err := readAll(d, user, e.Path)
	if err != nil || copyContent != "from phone" {
		t.Fatalf("conflict copy content: %q %v", copyContent, err)
	}
	_ = sha
}

// TestConflictRenameSameSecondNoClobber reproduces the reviewed HIGH finding:
// two uploads with identical basename + deviceTag inside the same second
// synthesize the SAME conflict name (stem + tag + second-stamp), and os.Rename
// would silently REPLACE the first conflict copy. Both copies must survive
// with distinct names and intact contents.
func TestConflictRenameSameSecondNoClobber(t *testing.T) {
	d := newTestDriver(t)
	const user = "kate"
	if _, _, _, _, err := d.Upload(user, "/f.txt", strings.NewReader("original"), 1<<20, "", false, ""); err != nil {
		t.Fatal(err)
	}
	sha1, e1, c1, _, err := d.Upload(user, "/f.txt", strings.NewReader("first-conflict"), 1<<20, "", true, "pixel-9")
	if err != nil || !c1 {
		t.Fatalf("first conflict upload: conflicted=%v err=%v", c1, err)
	}
	sha2, e2, c2, _, err := d.Upload(user, "/f.txt", strings.NewReader("second-conflict"), 1<<20, "", true, "pixel-9")
	_ = sha2
	if err != nil || !c2 {
		t.Fatalf("second conflict upload: conflicted=%v err=%v", c2, err)
	}
	_ = sha1

	paths := []string{"/f.txt", e1.Path, e2.Path}
	distinct := map[string]bool{}
	for _, p := range paths {
		if distinct[p] {
			t.Fatalf("collision: two of %v share path %q — earlier conflict copy destroyed", paths, p)
		}
		distinct[p] = true
	}
	want := map[string]string{"/f.txt": "original", e1.Path: "first-conflict", e2.Path: "second-conflict"}
	for p, content := range want {
		got, _, _, err := readAll(d, user, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if got != content {
			t.Fatalf("%s: got %q, want %q", p, got, content)
		}
	}
}

func readAll(d *Driver, user, path string) (string, int64, string, error) {
	f, fi, logical, err := d.OpenFile(user, path)
	if err != nil {
		return "", 0, "", err
	}
	defer f.Close()
	buf := make([]byte, fi.Size())
	n, _ := f.Read(buf)
	return string(buf[:n]), fi.Size(), logical, nil
}

func TestMoveRenameGuards(t *testing.T) {
	d := newTestDriver(t)
	const user = "dave"
	d.Mkdir(user, "/a")
	d.Mkdir(user, "/a/sub")
	if _, err := d.Move(user, "/a", "/a/sub"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("move into self must fail, got %v", err)
	}
	if _, err := d.Rename(user, "/", "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rename root must fail, got %v", err)
	}
	if _, err := d.Rename(user, "/a", "../escape"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad name must fail, got %v", err)
	}
}

func TestSearchAndWalk(t *testing.T) {
	d := newTestDriver(t)
	const user = "eve"
	d.Mkdir(user, "/pics")
	d.Upload(user, "/pics/cat photo.JPG", strings.NewReader("x"), 1<<20, "", false, "")
	d.Upload(user, "/notes.txt", strings.NewReader("y"), 1<<20, "", false, "")
	res, err := d.Search(user, "CAT", 10)
	if err != nil || len(res) != 1 || res[0].Name != "cat photo.JPG" {
		t.Fatalf("search: %+v err=%v", res, err)
	}
	var seen int
	err = d.WalkTree(user, "/", func(e *Entry, _ string) error { seen++; return nil })
	if err != nil || seen < 4 { // root dir, pics dir, 2 files
		t.Fatalf("walk: seen=%d err=%v", seen, err)
	}
}

// TestResolveUserPathRejectsBadUsernames proves the pre-clean containment
// check: a hostile username can never point resolution outside files/.
func TestResolveUserPathRejectsBadUsernames(t *testing.T) {
	d := newTestDriver(t)
	for _, u := range []string{"..", ".", "../alice", "a/../b", "", "alice/", "./alice"} {
		abs, logical, err := d.ResolveUserPath(u, "/x.txt")
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("username %q: want ErrInvalid, got abs=%q logical=%q err=%v", u, abs, logical, err)
		}
	}
	// Legitimate dotted usernames must keep working.
	os.MkdirAll(filepath.Join(d.Root(), "files", "ada.lovelace"), 0o700)
	if abs, _, err := d.ResolveUserPath("ada.lovelace", "/x.txt"); err != nil || abs == "" {
		t.Errorf("legit username rejected: abs=%q err=%v", abs, err)
	}
}

// TestDeleteKeepsOriginalWhenTrashMetaFails simulates a metadata-write failure
// (read-only trash directory) and asserts the original file is still at its
// path and no half-state is visible.
func TestDeleteKeepsOriginalWhenTrashMetaFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission bits are not enforced")
	}
	d := newTestDriver(t)
	const user = "frank"
	if _, _, _, _, err := d.Upload(user, "/keep.txt", strings.NewReader("precious"), 1<<20, "", false, ""); err != nil {
		t.Fatal(err)
	}

	trashDir := filepath.Join(d.Root(), "trash", user)
	if err := os.MkdirAll(trashDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(trashDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(trashDir, 0o700) })

	if id, orig, err := d.Delete(user, "/keep.txt"); err == nil {
		t.Fatalf("expected metadata-write failure, got id=%q orig=%q", id, orig)
	}

	// Original must survive untouched.
	data, _, _, err := readAll(d, user, "/keep.txt")
	if err != nil || data != "precious" {
		t.Fatalf("original lost after failed delete: content=%q err=%v", data, err)
	}
	// No half-state in the trash listing.
	items, err := d.ListTrash(user)
	if err != nil || len(items) != 0 {
		t.Fatalf("half-state listed by ListTrash: %+v err=%v", items, err)
	}
	// No leftover .tmp sidecars on disk either.
	ents, _ := os.ReadDir(trashDir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover sidecar in trash dir: %q", e.Name())
		}
	}
}

// TestPurgeOldTrashSkipsUnsafeMetadata pins the janitor safety rules:
// corrupt JSON is skipped (payload intact), deletedAt=0 is skipped,
// plausible-but-older-than-cutoff is purged, newer is kept.
func TestPurgeOldTrashSkipsUnsafeMetadata(t *testing.T) {
	d := newTestDriver(t)
	const user = "gina"
	dir := filepath.Join(d.Root(), "trash", user)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(id, metaJSON, payload string) {
		if payload != "" {
			if err := os.WriteFile(filepath.Join(dir, id), []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if metaJSON != "" {
			if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(metaJSON), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return !errors.Is(err, os.ErrNotExist)
	}

	corruptID := strings.Repeat("a", 32)
	zeroID := strings.Repeat("b", 32)
	agedID := strings.Repeat("c", 32)
	freshID := strings.Repeat("d", 32)

	now := time.Now()
	old := now.Add(-48 * time.Hour).Unix()
	fresh := now.Add(-1 * time.Hour).Unix()

	write(corruptID, "{definitely not json", "corrupt-payload")
	write(zeroID, `{"deletedAt":0}`, "epoch-payload")
	write(agedID, `{"deletedAt":`+fmt.Sprint(old)+`,"origPath":"/aged.txt"}`, "aged-payload")
	write(freshID, `{"deletedAt":`+fmt.Sprint(fresh)+`}`, "fresh-payload")

	purged := d.PurgeOldTrash(now.Add(-24 * time.Hour))
	if len(purged) != 1 {
		t.Fatalf("purged %d items; want exactly 1 (the aged one)", len(purged))
	}
	if purged[0].Username != user || purged[0].OrigPath != "/aged.txt" {
		t.Fatalf("reported purge = %+v; want {Username:%q OrigPath:%q}", purged[0], user, "/aged.txt")
	}

	// Corrupt + epoch-zero survive with payloads AND metadata intact.
	if !exists(corruptID) || !exists(corruptID+".json") {
		t.Error("corrupt-metadata item was purged; it must be skipped")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, corruptID)); string(got) != "corrupt-payload" {
		t.Errorf("corrupt item payload damaged: %q", got)
	}
	if !exists(zeroID) {
		t.Error("deletedAt=0 item was purged; implausible timestamps must be skipped")
	}
	// Aged item fully gone; fresh item kept.
	if exists(agedID) || exists(agedID+".json") {
		t.Error("aged item was not purged")
	}
	if !exists(freshID) {
		t.Error("fresh item must be kept")
	}
}

// TestListAndWalkSkipStagingFiles checks .xxpart / .xxpartial parity between
// List and WalkTree.
func TestListAndWalkSkipStagingFiles(t *testing.T) {
	d := newTestDriver(t)
	const user = "henry"
	base := filepath.Join(d.Root(), "files", user)
	os.MkdirAll(filepath.Join(base, "sub"), 0o755)
	if _, _, _, _, err := d.Upload(user, "/notes.txt", strings.NewReader("n"), 1<<20, "", false, ""); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(base, "foo.xxpart"), []byte("p"), 0o600)
	os.WriteFile(filepath.Join(base, ".xxpartial-abc123"), []byte("p"), 0o600)
	os.WriteFile(filepath.Join(base, "sub", "bar.xxpart"), []byte("p"), 0o600)

	ents, err := d.List(user, "/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name)
		if isStagingEntry(e.Name) {
			t.Errorf("List exposed staging entry %q", e.Name)
		}
	}
	for _, want := range []string{"notes.txt", "sub"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("List missing real entry %q; got %v", want, names)
		}
	}

	var walked []string
	if err := d.WalkTree(user, "/", func(e *Entry, _ string) error {
		walked = append(walked, e.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(walked, ",")
	if strings.Contains(joined, "xxpart") || strings.Contains(joined, "xxpartial") {
		t.Fatalf("WalkTree visited staging entries: %v", walked)
	}
	if !strings.Contains(joined, "/notes.txt") || !strings.Contains(joined, "/sub") {
		t.Fatalf("WalkTree missed real entries: %v", walked)
	}
}

// TestVersionBlobLockSmoke hammers OpenVersion concurrently with both blob
// removal APIs (janitor prune + version-dir purge) to exercise the
// VersionBlobMu read/write discipline under the race detector. It is a smoke
// test, not a proof: the invariant (no removal between resolve and open) is
// enforced by lock placement, reviewed at the call sites.
func TestVersionBlobLockSmoke(t *testing.T) {
	d := newTestDriver(t)
	const user = "iris"
	if _, _, _, _, err := d.Upload(user, "/f.txt", strings.NewReader("v1"), 1<<20, "", false, ""); err != nil {
		t.Fatal(err)
	}
	vid, _, err := d.SnapshotCurrent(user, "/f.txt")
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			f, _, err := d.OpenVersion(user, "/f.txt", vid)
			if err == nil {
				f.Close()
			} else if err != ErrNotFound {
				t.Errorf("OpenVersion: unexpected error: %v", err)
				return
			}
		}
	}()

	blob := func(content string) {
		if err := os.MkdirAll(filepath.Dir(d.VersionBlobPath(user, "/f.txt", vid)), 0o700); err != nil {
			t.Error(err)
		}
		os.WriteFile(d.VersionBlobPath(user, "/f.txt", vid), []byte(content), 0o600)
	}
	for i := 0; i < 100; i++ {
		blob("x")
		if err := d.PruneVersionBlob(user, "/f.txt", vid); err != nil {
			t.Fatalf("PruneVersionBlob: %v", err)
		}
		blob("y")
		if err := d.DeleteVersionDir(user, "/f.txt"); err != nil {
			t.Fatalf("DeleteVersionDir: %v", err)
		}
	}
	close(stop)
	<-done

	if _, err := os.Stat(d.VersionBlobPath(user, "/f.txt", vid)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version blob survived purge: %v", err)
	}
}

func TestRemoveUserDeletesTreesAndRejectsHostileNames(t *testing.T) {
	d := newTestDriver(t)
	if _, _, _, _, err := d.Upload("alice", "/a.txt", strings.NewReader("hi"), 1<<20, "", false, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := d.RemoveUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.Root(), "files", "alice")); !os.IsNotExist(err) {
		t.Fatalf("files/alice survived: %v", err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a\\b"} {
		if err := d.RemoveUser(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("RemoveUser(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}
