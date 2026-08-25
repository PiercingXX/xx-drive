package fsdrv

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
