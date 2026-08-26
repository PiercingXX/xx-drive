package main

import (
	"path/filepath"
	"strings"
	"testing"

	"xxdrive/internal/store"
)

// TestSetUserPassword verifies the `-passwd` recovery path end to end against
// a real temp SQLite store: validation, upsert, and hash compatibility with
// store.CheckPassword (the same verifier the login endpoint uses).
func TestSetUserPassword(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "xxdrive.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	const oldPW = "original-password"
	if _, err := st.CreateUser("admin", store.HashPassword(oldPW), true); err != nil {
		t.Fatalf("create user: %v", err)
	}

	u, err := st.UserByName("admin")
	if err != nil {
		t.Fatalf("lookup user: %v", err)
	}
	if !store.CheckPassword(u.PasswordHash, oldPW) || store.CheckPassword(u.PasswordHash, "brand-new-pw") {
		t.Fatal("precondition: old password should verify, new one should not")
	}

	// Too short — same minimum the HTTP password endpoints enforce.
	err = setUserPassword(st, "admin", "short")
	if err == nil || !strings.Contains(err.Error(), "8") {
		t.Fatalf("setUserPassword(short) = %v; want minimum-length error", err)
	}

	// Unknown user.
	if err := setUserPassword(st, "ghost", "long-enough-password"); err == nil {
		t.Fatal("setUserPassword(ghost) = nil; want unknown-user error")
	}

	// Happy path.
	if err := setUserPassword(st, "admin", "brand-new-pw"); err != nil {
		t.Fatalf("setUserPassword: %v", err)
	}

	fresh, err := st.UserByName("admin")
	if err != nil {
		t.Fatalf("re-lookup user: %v", err)
	}
	if !store.CheckPassword(fresh.PasswordHash, "brand-new-pw") {
		t.Error("new password does not verify after reset")
	}
	if store.CheckPassword(fresh.PasswordHash, oldPW) {
		t.Error("old password still verifies after reset")
	}
}
