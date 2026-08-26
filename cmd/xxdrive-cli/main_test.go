package main

import (
	"encoding/hex"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestBasePathForStableSha256Hex(t *testing.T) {
	old := cfgPath
	cfgPath = filepath.Join(t.TempDir(), "config.json")
	defer func() { cfgPath = old }()

	// Includes inputs shorter than 12 chars — the old byte-fold + [:24] scheme
	// panicked on these.
	pairs := [][2]string{{"", ""}, {"/d", "/x"}, {"/media/Working-Storage/sync", "/docs folder"}}
	for _, pair := range pairs {
		p1 := basePathFor(pair[0], pair[1])
		p2 := basePathFor(pair[0], pair[1])
		if p1 != p2 {
			t.Fatalf("unstable baseline path for %v: %s vs %s", pair, p1, p2)
		}
		name := filepath.Base(p1)
		mid := strings.TrimSuffix(strings.TrimPrefix(name, "sync-"), ".json")
		if len(mid) != 64 {
			t.Fatalf("want full 64-char sha256 hex, got %d chars: %q", len(mid), mid)
		}
		if _, err := hex.DecodeString(mid); err != nil {
			t.Fatalf("not hex: %v", err)
		}
	}

	if basePathFor("/a", "/b") == basePathFor("/a", "/c") {
		t.Fatal("different pairs must not share one baseline file")
	}
	if basePathFor("/a/b", "/c") == basePathFor("/a", "/b/c") {
		t.Fatal("pair separator lost: distinct pairs collide")
	}
}

func TestWalkLocalSkipsPartialDownloads(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, filepath.Join(root, "a.txt"), "a")
	writeLocal(t, filepath.Join(root, "photo.jpg.xxpart"), "interrupted")
	writeLocal(t, filepath.Join(root, ".xxpartialleftover"), "legacy partial")
	writeLocal(t, filepath.Join(root, "sub", "c.txt"), "c")
	writeLocal(t, filepath.Join(root, ".xxdrive-trash", "20260101T000000", "y.bin"), "trashed")
	writeLocal(t, filepath.Join(root, ".xxpart", "inside.txt"), "legacy staging dir")

	got, err := walkLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/a.txt", "/sub/c.txt"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("missing %s in %v", want, got)
		}
	}
	for _, unwanted := range []string{
		"/photo.jpg.xxpart", "/.xxpartialleftover",
		"/.xxdrive-trash/20260101T000000/y.bin", "/.xxpart/inside.txt",
	} {
		if _, ok := got[unwanted]; ok {
			t.Fatalf("walkLocal must skip %s, got %v", unwanted, got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 entries, got %v", got)
	}
}

func TestCmdUpOverwritesAndVersions(t *testing.T) {
	env := newSrvEnv(t)
	env.installCfg(t)

	env.putRemote(t, "/up/doc.txt", "server original")
	local := filepath.Join(t.TempDir(), "doc.txt")
	writeLocal(t, local, "client replacement")

	// `up` defaults to overwrite+version: the remote is replaced in place,
	// no conflict-renamed sibling appears.
	if err := cmdUp([]string{local, "/up/doc.txt"}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	if got := env.fetchRemote(t, "/up/doc.txt"); got != "client replacement" {
		t.Fatalf("up did not overwrite remote: %q", got)
	}

	// The previous content was snapshotted as a version.
	// (/api/versions answers with a bare JSON array, so bypass doJSON.)
	vreq, err := http.NewRequest("GET", env.baseURL+"/api/versions?path="+urlEscape("/up/doc.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	vreq.Header.Set("Authorization", "Bearer "+env.token)
	vresp, err := http.DefaultClient.Do(vreq)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(vresp.Body)
	vresp.Body.Close()
	if !strings.Contains(string(raw), "versionId") {
		t.Fatalf("no version snapshot after overwrite: %s", raw)
	}

	ents, _ := listDir(env.cfg(), "/up")
	if len(ents) != 1 || ents[0].Name != "doc.txt" {
		t.Fatalf("expected exactly doc.txt after up, got %v", ents)
	}
}
