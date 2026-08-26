package api

// Tests for the P1 server-hardening fixes that don't fit the older files:
// login timing equalization and the X-Requested-With CSRF signal.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"xxdrive/internal/store"
)

// TestLoginUnknownUserBurnsHash pins the anti-enumeration behavior: a login
// attempt for a nonexistent username must invoke the dummy PBKDF2 seam
// (same cost class as CheckPassword on a real record), while known users —
// right or wrong password — go through the real verification only. The seam
// is asserted deterministically instead of timing batches.
func TestLoginUnknownUserBurnsHash(t *testing.T) {
	env := newTestEnv(t)
	carol, err := env.st.CreateUser("carol", store.HashPassword("correcthorse"), false)
	if err != nil {
		t.Fatal(err)
	}
	dave, err := env.st.CreateUser("dave", store.HashPassword("correcthorse"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.st.SetDisabled(dave.ID, true); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		username string
		password string
		wantCode int
		wantBurn int
	}{
		{"unknown username burns dummy hash", "nosuchuser", "whatever", http.StatusUnauthorized, 1},
		{"second unknown username burns again", "also-missing", "whatever", http.StatusUnauthorized, 1},
		{"known user wrong password uses real check", "carol", "wrongpass", http.StatusUnauthorized, 0},
		{"disabled account burns dummy hash too", dave.Username, "wrongpass", http.StatusUnauthorized, 1},
		{"successful login uses real check", carol.Username, "correcthorse", http.StatusOK, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			burns := 0
			orig := dummyHash
			dummyHash = func(string) []byte { burns++; return nil }
			defer func() { dummyHash = orig }()

			body, _ := json.Marshal(map[string]string{"username": tc.username, "password": tc.password})
			resp, b := env.req("POST", "/api/auth/login", "", bytes.NewReader(body), jsonHdr())
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.wantCode, b)
			}
			if burns != tc.wantBurn {
				t.Fatalf("dummy PBKDF2 invocations = %d, want %d", burns, tc.wantBurn)
			}
		})
	}
}

// TestAdminMutationCSRF pins finding: admin WRITES must pass the same
// Origin/Referer CSRF policy as every other mutating route. A cross-origin
// POST with a stolen admin cookie is rejected, while the bearer-token CLI
// path (no Origin, no ambient credentials) keeps working.
func TestAdminMutationCSRF(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.st.CreateUser("root", store.HashPassword("adminpass123"), true); err != nil {
		t.Fatal(err)
	}
	adminTok := env.login("root", "adminpass123")

	req, _ := http.NewRequest("POST", env.baseURL+"/api/admin/users",
		strings.NewReader(`{"username":"pwned","password":"pwnpass123","isAdmin":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: adminTok})
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin admin POST with valid admin cookie: got %d, want 403", resp.StatusCode)
	}
	if _, err := env.st.UserByName("pwned"); err == nil {
		t.Fatal("cross-origin admin request created a user")
	}

	// the other two admin mutations reject the same way
	for _, path := range []string{"/api/admin/users/set-state", "/api/admin/users/password"} {
		req2, _ := http.NewRequest("POST", env.baseURL+path,
			strings.NewReader(`{"username":"root","disabled":true,"password":"whatever123"}`))
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("Origin", "https://evil.example")
		req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: adminTok})
		r2, err := env.client.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, r2.Body)
		r2.Body.Close()
		if r2.StatusCode != http.StatusForbidden {
			t.Fatalf("%s cross-origin: got %d, want 403", path, r2.StatusCode)
		}
	}

	// bearer-token admin mutation (CLI style) still permitted
	resp3, b := postJSON(env, adminTok, "/api/admin/users",
		map[string]any{"username": "climade", "password": "clipass123", "isAdmin": false})
	if resp3.StatusCode != 200 || !strings.Contains(string(b), "ok") {
		t.Fatalf("bearer admin POST should still work: %d %s", resp3.StatusCode, b)
	}
}

// TestCSRFRequestedWithHeader pins the additive CSRF signal: a cookie-only
// mutating request with no Origin/Referer passes when it carries
// X-Requested-With (deliberate non-browser client), but a present-but-foreign
// Origin stays rejected even WITH the header.
func TestCSRFRequestedWithHeader(t *testing.T) {
	env := newTestEnv(t)
	tok := env.mkUser("erin")

	attempt := func(path string, hdr map[string]string) int {
		t.Helper()
		req, err := http.NewRequest("POST", env.baseURL+"/api/files/mkdir",
			strings.NewReader(`{"path":"`+path+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no origin + header is allowed", map[string]string{"X-Requested-With": "XMLHttpRequest"}, http.StatusOK},
		{"no origin + no header stays rejected", nil, http.StatusForbidden},
		{
			"cross-origin origin rejected even with header",
			map[string]string{"X-Requested-With": "XMLHttpRequest", "Origin": "https://evil.example"},
			http.StatusForbidden,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := strings.Repeat("/x", i+1)
			if got := attempt(path, tc.hdr); got != tc.want {
				t.Fatalf("mkdir status = %d, want %d", got, tc.want)
			}
		})
	}
}
