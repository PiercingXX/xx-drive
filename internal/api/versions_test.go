package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- shared helpers for the version-history tests ----

type verRow struct {
	VersionID string `json:"versionId"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"createdAt"`
}

func mustUpload(t *testing.T, env *testEnv, tok, path, content string) {
	t.Helper()
	resp, out := env.upload(tok, path, content, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("upload %s: %d %v", path, resp.StatusCode, out)
	}
}

func fetchVersions(t *testing.T, env *testEnv, tok, path string) []verRow {
	t.Helper()
	resp, b := env.req("GET", "/api/versions?path="+url.QueryEscape(path), tok, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list versions %s: %d %s", path, resp.StatusCode, b)
	}
	var vs []verRow
	if err := json.Unmarshal(b, &vs); err != nil {
		t.Fatalf("versions %s: bad json %s", path, b)
	}
	return vs
}

func mustDownload(t *testing.T, env *testEnv, tok, path string) string {
	t.Helper()
	resp, b := env.req("GET", "/api/files/download?path="+url.QueryEscape(path), tok, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("download %s: %d", path, resp.StatusCode)
	}
	return string(b)
}

// restoreNewestVersion restores the single newest version of path and fails
// the test on any non-200.
func restoreNewestVersion(t *testing.T, env *testEnv, tok, path string) {
	t.Helper()
	vs := fetchVersions(t, env, tok, path)
	if len(vs) == 0 {
		t.Fatalf("no versions to restore for %s", path)
	}
	resp, b := postJSON(env, tok, "/api/versions/restore",
		map[string]string{"path": path, "versionId": vs[0].VersionID})
	if resp.StatusCode != 200 {
		t.Fatalf("version restore %s @ %s: %d %s", path, vs[0].VersionID, resp.StatusCode, b)
	}
}

// ---- item 1: rename/move follows history ----

func TestRenameFollowsVersionHistory(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")

	mustUpload(t, env, tok, "/r/a.txt", "v1")
	mustUpload(t, env, tok, "/r/a.txt", "v2") // snapshots v1

	resp, b := postJSON(env, tok, "/api/files/rename", map[string]string{"path": "/r/a.txt", "newName": "b.txt"})
	if resp.StatusCode != 200 {
		t.Fatalf("rename: %d %s", resp.StatusCode, b)
	}

	// versions now live under the NEW path only
	if got := fetchVersions(t, env, tok, "/r/b.txt"); len(got) != 1 {
		t.Fatalf("expected 1 version under new path, got %d", len(got))
	}
	if got := fetchVersions(t, env, tok, "/r/a.txt"); len(got) != 0 {
		t.Fatalf("old path still lists %d versions", len(got))
	}

	// restoring under the new path yields v1 bytes and stays reversible
	restoreNewestVersion(t, env, tok, "/r/b.txt")
	if got := mustDownload(t, env, tok, "/r/b.txt"); got != "v1" {
		t.Fatalf("after restore want %q, got %q", "v1", got)
	}
	if got := fetchVersions(t, env, tok, "/r/b.txt"); len(got) != 2 {
		t.Fatalf("restore should snapshot prior current (want 2 versions), got %d", len(got))
	}
}

func TestDirectoryMoveRelocatesChildVersions(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")

	for _, p := range []string{"/proj", "/archive"} {
		if resp, b := postJSON(env, tok, "/api/files/mkdir", map[string]string{"path": p}); resp.StatusCode != 200 {
			t.Fatalf("mkdir %s: %d %s", p, resp.StatusCode, b)
		}
	}
	mustUpload(t, env, tok, "/proj/sub/f.txt", "v1")
	mustUpload(t, env, tok, "/proj/sub/f.txt", "v2") // snapshots v1

	resp, b := postJSON(env, tok, "/api/files/move", map[string]string{"path": "/proj", "destDir": "/archive"})
	if resp.StatusCode != 200 {
		t.Fatalf("move: %d %s", resp.StatusCode, b)
	}

	newPath := "/archive/proj/sub/f.txt"
	if got := fetchVersions(t, env, tok, newPath); len(got) != 1 {
		t.Fatalf("child versions not relocated (want 1 at %s), got %d", newPath, len(got))
	}
	if got := fetchVersions(t, env, tok, "/proj/sub/f.txt"); len(got) != 0 {
		t.Fatalf("old prefix still lists %d child versions", len(got))
	}

	// blob dir really moved too: restore serves the old bytes
	restoreNewestVersion(t, env, tok, newPath)
	if got := mustDownload(t, env, tok, newPath); got != "v1" {
		t.Fatalf("restored moved-child want %q, got %q", "v1", got)
	}
}

// ---- item 2: trash keeps history until permanent purge ----

func TestTrashPreservesHistoryAndRestoreBringsItBack(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")

	mustUpload(t, env, tok, "/t/note.txt", "v1")
	mustUpload(t, env, tok, "/t/note.txt", "v2") // version = v1

	resp, _ := postJSON(env, tok, "/api/files/delete", map[string]string{"path": "/t/note.txt"})
	if resp.StatusCode != 200 {
		t.Fatalf("trash: %d", resp.StatusCode)
	}

	// while trashed: index rows AND blobs survive (this used to be dropped)
	if got := fetchVersions(t, env, tok, "/t/note.txt"); len(got) != 1 {
		t.Fatalf("trash dropped version index (want 1 row), got %d", len(got))
	}

	resp, b := env.req("GET", "/api/trash", tok, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "note.txt") {
		t.Fatalf("trash list: %d %s", resp.StatusCode, b)
	}
	var items []struct {
		ID string `json:"id"`
	}
	json.Unmarshal(b, &items)
	if len(items) == 0 {
		t.Fatal("trash empty")
	}
	resp, b = postJSON(env, tok, "/api/trash/restore", map[string]string{"id": items[0].ID})
	if resp.StatusCode != 200 {
		t.Fatalf("restore: %d %s", resp.StatusCode, b)
	}

	// restored file lists its history again and a version restore works
	if got := fetchVersions(t, env, tok, "/t/note.txt"); len(got) != 1 {
		t.Fatalf("history lost across trash round-trip (want 1), got %d", len(got))
	}
	if got := mustDownload(t, env, tok, "/t/note.txt"); got != "v2" {
		t.Fatalf("restored current bytes wrong: %q", got)
	}
	restoreNewestVersion(t, env, tok, "/t/note.txt")
	if got := mustDownload(t, env, tok, "/t/note.txt"); got != "v1" {
		t.Fatalf("version restore after trash round-trip want %q, got %q", "v1", got)
	}
}

func TestTrashPurgeDropsRowsAndBlobs(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")
	userVerDir := filepath.Join(env.fs.Root(), "versions", "alice")

	trashOne := func(path string) string {
		t.Helper()
		mustUpload(t, env, tok, path, "v1")
		mustUpload(t, env, tok, path, "v2") // version = v1
		resp, _ := postJSON(env, tok, "/api/files/delete", map[string]string{"path": path})
		if resp.StatusCode != 200 {
			t.Fatalf("trash %s: %d", path, resp.StatusCode)
		}
		resp, b := env.req("GET", "/api/trash", tok, nil, nil)
		var items []struct {
			ID       string `json:"id"`
			OrigPath string `json:"origPath"`
		}
		json.Unmarshal(b, &items)
		for _, it := range items {
			if it.OrigPath == path {
				return it.ID
			}
		}
		t.Fatalf("trashed %s not in trash list: %s", path, b)
		return ""
	}

	// permanent single-item purge removes rows AND blob dirs
	id := trashOne("/p/gone.txt")
	if resp, b := postJSON(env, tok, "/api/trash/delete", map[string]string{"id": id}); resp.StatusCode != 200 {
		t.Fatalf("purge: %d %s", resp.StatusCode, b)
	}
	if got := fetchVersions(t, env, tok, "/p/gone.txt"); len(got) != 0 {
		t.Fatalf("rows survived purge: %d", len(got))
	}
	if ents, err := os.ReadDir(userVerDir); err != nil && !os.IsNotExist(err) {
		t.Fatalf("read versions dir: %v", err)
	} else if err == nil && len(ents) != 0 {
		t.Fatalf("blob dirs survived purge: %d left", len(ents))
	}

	// empty-trash purge does the same
	id2 := trashOne("/p/two.txt")
	if id2 == "" {
		t.Fatal("empty trash id")
	}
	if resp, b := postJSON(env, tok, "/api/trash/empty", nil); resp.StatusCode != 200 {
		t.Fatalf("empty trash: %d %s", resp.StatusCode, b)
	}
	if got := fetchVersions(t, env, tok, "/p/two.txt"); len(got) != 0 {
		t.Fatalf("rows survived empty-trash: %d", len(got))
	}
	if ents, err := os.ReadDir(userVerDir); err == nil && len(ents) != 0 {
		t.Fatalf("blob dirs survived empty-trash: %d left", len(ents))
	}
}

// TestTrashRestoreSplitsHistoryFromRecreatedFile pins the reviewed MEDIUM
// fix: version rows are keyed by logical path, so after trash → recreate →
// overwrite the SAME path holds rows from TWO different files. Restoring the
// old trash item to a sibling must move ONLY the rows captured before
// restore — never the newer file's rows, and blob files must follow.
func TestTrashRestoreSplitsHistoryFromRecreatedFile(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")

	// old file's life: v1 then v2 (v1 snapshotted as a version row)
	mustUpload(t, env, tok, "/t/doc.txt", "old-v1")
	mustUpload(t, env, tok, "/t/doc.txt", "old-v2")
	oldVers := fetchVersions(t, env, tok, "/t/doc.txt")
	if len(oldVers) != 1 {
		t.Fatalf("setup: want 1 old version, got %d", len(oldVers))
	}
	oldVid := oldVers[0].VersionID

	// trash it
	resp, _ := postJSON(env, tok, "/api/files/delete", map[string]string{"path": "/t/doc.txt"})
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	// recreate the same path with a NEW file and give it its own version.
	// The path now temporarily holds BOTH files' rows (trash keeps history).
	mustUpload(t, env, tok, "/t/doc.txt", "new-v1")
	mustUpload(t, env, tok, "/t/doc.txt", "new-v2")
	both := fetchVersions(t, env, tok, "/t/doc.txt")
	if len(both) != 2 {
		t.Fatalf("path should hold old+new rows pre-restore, got %d", len(both))
	}
	var newVid string
	for _, v := range both {
		if v.VersionID != oldVid {
			newVid = v.VersionID
		}
	}
	if newVid == "" {
		t.Fatal("no distinct new version row found")
	}

	// restore the OLD trash item; path occupied → numbered sibling
	resp, b := env.req("GET", "/api/trash", tok, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("trash list: %d %s", resp.StatusCode, b)
	}
	var items []struct {
		ID       string `json:"id"`
		OrigPath string `json:"origPath"`
	}
	json.Unmarshal(b, &items)
	var trashID string
	for _, it := range items {
		if it.OrigPath == "/t/doc.txt" {
			trashID = it.ID
		}
	}
	if trashID == "" {
		t.Fatalf("old item missing from trash: %s", b)
	}
	resp, rb := postJSON(env, tok, "/api/trash/restore", map[string]string{"id": trashID})
	if resp.StatusCode != 200 {
		t.Fatalf("restore: %d %s", resp.StatusCode, rb)
	}
	var out struct {
		RestoredTo string `json:"restoredTo"`
	}
	json.Unmarshal(rb, &out)
	sibling := out.RestoredTo
	if sibling == "" || sibling == "/t/doc.txt" {
		t.Fatalf("expected numbered sibling destination, got %q", sibling)
	}

	// sibling carries EXACTLY the old file's version(s), no more
	sibVers := fetchVersions(t, env, tok, sibling)
	if len(sibVers) != 1 || sibVers[0].VersionID != oldVid {
		t.Fatalf("sibling versions = %+v, want exactly [%s]", sibVers, oldVid)
	}
	// original path keeps EXACTLY the new file's rows — not emptied, not merged
	origVers := fetchVersions(t, env, tok, "/t/doc.txt")
	if len(origVers) != 1 || origVers[0].VersionID != newVid {
		t.Fatalf("original-path versions = %+v, want exactly [%s]", origVers, newVid)
	}

	// blobs followed their rows: sibling serves OLD bytes via its version,
	// original still serves ITS new bytes
	resp, vb := env.req("GET",
		"/api/versions/download?path="+url.QueryEscape(sibling)+"&versionId="+oldVid, tok, nil, nil)
	if resp.StatusCode != 200 || string(vb) != "old-v1" {
		t.Fatalf("sibling version blob: %d %q (blob did not follow rows)", resp.StatusCode, vb)
	}
	resp, vb = env.req("GET",
		"/api/versions/download?path=%2Ft%2Fdoc.txt&versionId="+newVid, tok, nil, nil)
	if resp.StatusCode != 200 || string(vb) != "new-v1" {
		t.Fatalf("original version blob damaged: %d %q", resp.StatusCode, vb)
	}

	// current content sanity: both files alive with their own bytes
	if got := mustDownload(t, env, tok, "/t/doc.txt"); got != "new-v2" {
		t.Fatalf("recreated file content wrong: %q", got)
	}
	if got := mustDownload(t, env, tok, sibling); got != "old-v2" {
		t.Fatalf("restored sibling content wrong: %q", got)
	}
}

// ---- item 3: zip must not follow planted symlinks ----

func TestZipSkipsPlantedSymlinks(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")

	mustUpload(t, env, tok, "/pics/real.txt", "regular-content")

	// secret OUTSIDE the data root + links planted via direct disk access
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("TOP-SECRET-OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	picsDir := filepath.Join(env.fs.Root(), "files", "alice", "pics")
	links := map[string]string{
		"leak.txt":     outside,                                     // symlink to external file
		"dangling.txt": filepath.Join(picsDir, "never-existed.bin"), // dangling link
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(picsDir, name)); err != nil {
			t.Skipf("symlinks unavailable on this filesystem: %v", err)
		}
	}

	resp, b := env.req("GET", "/api/files/zip?path=/pics", tok, nil, nil)
	if resp.StatusCode != 200 || len(b) < 4 || string(b[:2]) != "PK" {
		t.Fatalf("zip: %d magic=%q", resp.StatusCode, b[:min(4, len(b))])
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
			t.Fatalf("zip leaked planted symlink target via entry %q", f.Name)
		}
		switch strings.TrimPrefix(f.Name, "pics/") {
		case "real.txt":
			foundReal = string(body) == "regular-content"
		case "leak.txt", "dangling.txt":
			t.Fatalf("symlink entry included in zip: %q", f.Name)
		}
	}
	if !foundReal {
		t.Fatal("regular file missing from zip — walker over-skipped")
	}
}

// Purging a trash item must drop only THAT item's recorded versions. If a
// newer file was recreated at the same logical path after the trash, its
// history survives the old item's permanent purge.
func TestTrashPurgeSparesRecreatedFileHistory(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("alice")

	trashIDFor := func(path string) string {
		t.Helper()
		resp, b := env.req("GET", "/api/trash", tok, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("trash list: %d %s", resp.StatusCode, b)
		}
		var items []struct {
			ID       string `json:"id"`
			OrigPath string `json:"origPath"`
		}
		json.Unmarshal(b, &items)
		for _, it := range items {
			if it.OrigPath == path {
				return it.ID
			}
		}
		t.Fatalf("item for %s missing from trash: %s", path, b)
		return ""
	}

	// --- single-item purge ---
	mustUpload(t, env, tok, "/u/doc.txt", "old-v1")
	mustUpload(t, env, tok, "/u/doc.txt", "old-v2")
	oldRows := fetchVersions(t, env, tok, "/u/doc.txt")
	if len(oldRows) != 1 {
		t.Fatalf("setup: want 1 old version row, got %d", len(oldRows))
	}
	oldVid := oldRows[0].VersionID
	if resp, _ := postJSON(env, tok, "/api/files/delete", map[string]string{"path": "/u/doc.txt"}); resp.StatusCode != 200 {
		t.Fatal("delete old")
	}
	oldID := trashIDFor("/u/doc.txt")

	// recreate + overwrite: new file's own version row lands at the same key,
	// so pre-purge the path holds BOTH histories (old kept for restore)
	mustUpload(t, env, tok, "/u/doc.txt", "new-v1")
	mustUpload(t, env, tok, "/u/doc.txt", "new-v2")
	prePurge := fetchVersions(t, env, tok, "/u/doc.txt")
	if len(prePurge) != 2 {
		t.Fatalf("setup: want old+new rows pre-purge, got %d", len(prePurge))
	}
	var newVid string
	for _, v := range prePurge {
		if v.VersionID != oldVid {
			newVid = v.VersionID
		}
	}

	if resp, b := postJSON(env, tok, "/api/trash/delete", map[string]string{"id": oldID}); resp.StatusCode != 200 {
		t.Fatalf("purge old item: %d %s", resp.StatusCode, b)
	}
	if got := fetchVersions(t, env, tok, "/u/doc.txt"); len(got) != 1 || got[0].VersionID != newVid {
		t.Fatalf("recreated file history damaged by purge: %+v", got)
	}
	// and the surviving row still serves ITS bytes
	resp, vb := env.req("GET",
		"/api/versions/download?path="+url.QueryEscape("/u/doc.txt")+"&versionId="+newVid,
		tok, nil, nil)
	if resp.StatusCode != 200 || string(vb) != "new-v1" {
		t.Fatalf("surviving blob unreadable after purge: %d %q", resp.StatusCode, vb)
	}

	// --- empty-trash variant ---
	mustUpload(t, env, tok, "/w/doc.txt", "old2-v1")
	mustUpload(t, env, tok, "/w/doc.txt", "old2-v2")
	old2Rows := fetchVersions(t, env, tok, "/w/doc.txt")
	if len(old2Rows) != 1 {
		t.Fatalf("setup2: want 1 old version row, got %d", len(old2Rows))
	}
	old2Vid := old2Rows[0].VersionID
	if resp, _ := postJSON(env, tok, "/api/files/delete", map[string]string{"path": "/w/doc.txt"}); resp.StatusCode != 200 {
		t.Fatal("delete old2")
	}
	mustUpload(t, env, tok, "/w/doc.txt", "new2-v1")
	mustUpload(t, env, tok, "/w/doc.txt", "new2-v2")
	prePurge2 := fetchVersions(t, env, tok, "/w/doc.txt")
	if len(prePurge2) != 2 {
		t.Fatalf("setup2: want old+new rows pre-purge, got %d", len(prePurge2))
	}
	var new2Vid string
	for _, v := range prePurge2 {
		if v.VersionID != old2Vid {
			new2Vid = v.VersionID
		}
	}
	if resp, b := postJSON(env, tok, "/api/trash/empty", map[string]string{}); resp.StatusCode != 200 {
		t.Fatalf("empty trash: %d %s", resp.StatusCode, b)
	}
	if got := fetchVersions(t, env, tok, "/w/doc.txt"); len(got) != 1 || got[0].VersionID != new2Vid {
		t.Fatalf("recreated file history damaged by empty-trash: %+v", got)
	}
}
