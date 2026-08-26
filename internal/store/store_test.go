package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func TestCreateUserUsernameValidation(t *testing.T) {
	st, _ := openTestStore(t)
	hash := HashPassword("password123")

	for _, name := range []string{"ok", "ok2", "alice", "alice.smith", strings.Repeat("a", 64)} {
		if _, err := st.CreateUser(name, hash, false); err != nil {
			t.Errorf("CreateUser(%q) should accept: %v", name, err)
		}
	}
	for _, name := range []string{
		"", ".", "..",
		"a/b", "a\\b", "a\x00b",
		"/alice", "alice/", "a//b", "./alice", "a/./b", "a/../b",
		strings.Repeat("a", 65),
	} {
		if _, err := st.CreateUser(name, hash, false); err == nil {
			t.Errorf("CreateUser(%q) should reject", name)
		}
	}
	if n, err := st.CountUsers(); err != nil || n != 5 {
		t.Fatalf("CountUsers = %d, %v; want 5 (rejects must not create rows)", n, err)
	}
}

func TestGetOrCreateFabricUserValidation(t *testing.T) {
	st, _ := openTestStore(t)

	u, err := st.GetOrCreateFabricUser("10042")
	if err != nil {
		t.Fatalf("create fabric user: %v", err)
	}
	if u.Username != "fabric_10042" || u.FabricID != "10042" {
		t.Fatalf("shadow user mismatch: %+v", u)
	}
	u2, err := st.GetOrCreateFabricUser("10042")
	if err != nil || u2.ID != u.ID {
		t.Fatalf("second lookup should return same user: %+v err=%v", u2, err)
	}

	// Bad ids must be refused BEFORE being prefixed into a directory name.
	for _, bad := range []string{
		"", ".", "..",
		"a/b", "a\\b", "a\x00b",
		"../bob", "10042/", "./10042",
		strings.Repeat("x", 201),
	} {
		if _, err := st.GetOrCreateFabricUser(bad); err == nil {
			t.Errorf("GetOrCreateFabricUser(%q) should reject", bad)
		}
	}
	if n, _ := st.CountUsers(); n != 1 {
		t.Fatalf("CountUsers = %d; want 1 (rejected ids must not create shadow rows)", n)
	}
}

func TestSessionTokenHashedAtRest(t *testing.T) {
	st, path := openTestStore(t)
	u, err := st.CreateUser("sessie", HashPassword("password123"), false)
	if err != nil {
		t.Fatal(err)
	}
	raw := "raw-session-token-0123456789abcdef"
	if err := st.CreateSession(u.ID, raw, "web", time.Hour); err != nil {
		t.Fatal(err)
	}

	// Inspect the row through an independent connection: at rest the DB must
	// hold only the SHA-256 hash, never the raw token.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM sessions WHERE user_id = ?`, u.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != HashToken(raw) {
		t.Fatalf("at-rest token_hash = %q; want sha256 hex %q", stored, HashToken(raw))
	}
	if stored == raw || strings.Contains(stored, raw) || strings.Contains(raw, stored) {
		t.Fatalf("raw token leaked into storage: %q vs %q", stored, raw)
	}
	if _, _, err := st.Session(raw, time.Hour); err != nil {
		t.Fatalf("raw token must still authenticate: %v", err)
	}
	if _, _, err := st.Session("wrong-token", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong token should not authenticate, got %v", err)
	}
}

func TestShareRevokeInvalidatesToken(t *testing.T) {
	st, _ := openTestStore(t)
	u, err := st.CreateUser("sharer", HashPassword("password123"), false)
	if err != nil {
		t.Fatal(err)
	}
	token := "raw-share-token-zyxwvu"
	sh, err := st.CreateShare(u.ID, "/docs/report.txt", token, nil, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ShareByToken(token)
	if err != nil || got.TokenHash != sh.TokenHash || got.Path != "/docs/report.txt" {
		t.Fatalf("share by token before revoke: %+v err=%v", got, err)
	}

	if err := st.RevokeShare(u.ID, sh.TokenHash); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := st.ShareByToken(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked share must fail ShareByToken, got %v", err)
	}
	if _, err := st.ListShares(u.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteUserRemovesRowAndEtags(t *testing.T) {
	st, _ := openTestStore(t)
	u, err := st.CreateUser("goner", HashPassword("password123"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(u.ID, "raw-session-token", "web", time.Hour); err != nil {
		t.Fatal(err)
	}
	st.PutEtag(u.ID, "/x.txt", 1, 1, "deadbeef")
	if err := st.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserByName("goner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user row survived: %v", err)
	}
	if _, ok := st.CachedEtag(u.ID, "/x.txt", 1, 1); ok {
		t.Fatal("etag_cache row survived DeleteUser")
	}
	if _, _, err := st.Session("raw-session-token", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session survived cascade: %v", err)
	}
}
