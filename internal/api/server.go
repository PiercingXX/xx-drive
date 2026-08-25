// Package api wires HTTP handlers onto the store and filesystem driver.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"xxdrive/internal/fabric"
	"xxdrive/internal/fsdrv"
	"xxdrive/internal/store"
	"xxdrive/internal/webfs"
)

const sessionCookie = "xxd_session"
const csrfHeader = "X-Requested-With"

type Config struct {
	Addr               string
	DataDir            string
	BaseURL            string // e.g. https://drive.example.com — used in share links
	MaxUploadMB        int64
	SessionTTL         time.Duration
	TrashRetentionDays int
	TLSCert            string
	TLSKey             string

	nowFunc func() time.Time // test hook for token-expiry checks; nil means time.Now
}

type Server struct {
	cfg    Config
	st     *store.Store
	fs     *fsdrv.Driver
	ring   *fabric.Keyring // estate-SSO validator; nil when no keyring is configured
	mux    *http.ServeMux
	pubMu  sync.Mutex
	pubGr  map[string]map[string]int64 // tokenHash -> grant -> expiry
	rateMu sync.Mutex
	rate   map[string]*rateBucket
	userMu sync.Map // username -> *sync.Mutex (per-user mutation serialization)
}

// New builds the server. ring may be nil: when no estate keyring is configured
// the fabric-SSO paths report "not configured" and only the local
// admin/password auth is served, so the operator is never locked out.
func New(cfg Config, st *store.Store, fs *fsdrv.Driver, ring *fabric.Keyring) *Server {
	if cfg.MaxUploadMB <= 0 {
		cfg.MaxUploadMB = 10240
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.TrashRetentionDays <= 0 {
		cfg.TrashRetentionDays = 30
	}
	s := &Server{cfg: cfg, st: st, fs: fs, ring: ring,
		mux: http.NewServeMux(), pubGr: map[string]map[string]int64{}, rate: map[string]*rateBucket{}}
	s.routes()
	return s
}

// now returns the moment used for token-expiry checks (test-overridable).
func (s *Server) now() time.Time {
	if s.cfg.nowFunc != nil {
		return s.cfg.nowFunc()
	}
	return time.Now()
}

func (s *Server) Handler() http.Handler { return s.recoverPanics(s.logReq(s.securityHeaders(s.mux))) }

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("ok")) })

	// auth
	m.HandleFunc("POST /api/auth/login", s.handleLogin)
	m.HandleFunc("POST /api/auth/fabric", s.handleFabricLogin)
	m.HandleFunc("POST /api/auth/logout", s.authed(s.handleLogout))
	m.HandleFunc("GET /api/auth/me", s.authed(s.handleMe))
	m.HandleFunc("GET /api/auth/sessions", s.authed(s.handleListSessions))
	m.HandleFunc("POST /api/auth/sessions/revoke-others", s.authed(s.handleRevokeOthers))

	// files
	m.HandleFunc("GET /api/files/list", s.authed(s.handleList))
	m.HandleFunc("POST /api/files/mkdir", s.authedMutating(s.handleMkdir))
	m.HandleFunc("POST /api/files/upload", s.authedMutating(s.handleUpload))
	m.HandleFunc("GET /api/files/download", s.authed(s.handleDownload))
	m.HandleFunc("GET /api/files/zip", s.authed(s.handleZip))
	m.HandleFunc("POST /api/files/rename", s.authedMutating(s.handleRename))
	m.HandleFunc("POST /api/files/move", s.authedMutating(s.handleMove))
	m.HandleFunc("POST /api/files/copy", s.authedMutating(s.handleCopy))
	m.HandleFunc("POST /api/files/delete", s.authedMutating(s.handleDelete))

	// trash
	m.HandleFunc("GET /api/trash", s.authed(s.handleTrashList))
	m.HandleFunc("POST /api/trash/restore", s.authedMutating(s.handleTrashRestore))
	m.HandleFunc("POST /api/trash/delete", s.authedMutating(s.handleTrashDelete))
	m.HandleFunc("POST /api/trash/empty", s.authedMutating(s.handleTrashEmpty))

	// versions
	m.HandleFunc("GET /api/versions", s.authed(s.handleVersionList))
	m.HandleFunc("POST /api/versions/restore", s.authedMutating(s.handleVersionRestore))
	m.HandleFunc("GET /api/versions/download", s.authed(s.handleVersionDownload))

	// search / stars / events
	m.HandleFunc("GET /api/search", s.authed(s.handleSearch))
	m.HandleFunc("POST /api/star/toggle", s.authedMutating(s.handleStarToggle))
	m.HandleFunc("GET /api/starred", s.authed(s.handleStarred))
	m.HandleFunc("GET /api/events", s.authed(s.handleEvents))

	// shares (authenticated management)
	m.HandleFunc("GET /api/shares", s.authed(s.handleShareList))
	m.HandleFunc("POST /api/shares", s.authedMutating(s.handleShareCreate))
	m.HandleFunc("DELETE /api/shares/{tokenHash}", s.authedMutating(s.handleShareRevoke))

	// public share access
	m.HandleFunc("GET /s/{token}", s.handlePublicPage)
	m.HandleFunc("POST /s/{token}", s.handlePublicPassword)
	m.HandleFunc("GET /s/{token}/list", s.handlePublicList)
	m.HandleFunc("GET /s/{token}/download", s.handlePublicDownload)

	// admin
	m.HandleFunc("GET /api/admin/users", s.adminOnly(s.handleAdminListUsers))
	m.HandleFunc("POST /api/admin/users", s.adminOnly(s.handleAdminCreateUser))
	m.HandleFunc("POST /api/admin/users/set-state", s.adminOnly(s.handleAdminSetState))
	m.HandleFunc("POST /api/admin/users/password", s.adminOnly(s.handleAdminSetPassword))

	// embedded web app (SPA) — catch-all for everything else
	m.Handle("GET /", webfs.Handler())
}

// ---- middleware ----

type ctxKey int

const userKey ctxKey = 1

func UserFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

// authed requires a valid session (cookie or bearer token).
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.authenticate(w, r)
		if u == nil {
			return // response already written
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

// authedMutating additionally enforces CSRF defenses for cookie-based callers
// (bearer-token clients like the CLI are exempt — no ambient credentials).
func (s *Server) authedMutating(next http.HandlerFunc) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && !s.sameOrigin(r) {
			writeErr(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		// serialize mutations per user
		mu, _ := s.userMu.LoadOrStore(UserFrom(r).Username, &sync.Mutex{})
		mu.(*sync.Mutex).Lock()
		defer mu.(*sync.Mutex).Unlock()
		next(w, r)
	})
}

func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
		if origin == "" {
			return false
		}
	}
	host := r.Host
	reqURL, err := parseOrigin(origin)
	if err != nil {
		return false
	}
	return reqURL == host || reqURL == hostOf(host)
}

func parseOrigin(origin string) (string, error) {
	o := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	if i := strings.IndexAny(o, "/?#"); i >= 0 {
		o = o[:i]
	}
	if o == "" {
		return "", errors.New("empty origin")
	}
	return o, nil
}

func hostOf(hostPort string) string {
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return h
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) *store.User {
	var raw string
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		raw = strings.TrimPrefix(h, "Bearer ")
	} else if c, err := r.Cookie(sessionCookie); err == nil {
		raw = c.Value
	}
	if raw == "" {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return nil
	}
	// A ClusterKeyring v1 token (v1.<key>.<body>.<sig>) is an estate-SSO
	// credential: validate it against the ring and resolve to the caller's
	// shadow user. Local opaque session tokens never contain dots, so this
	// dispatch is unambiguous. A malformed/expired/wrong-key fabric token is
	// rejected outright — it is never re-tried as a local session.
	if fabric.LooksLikeToken(raw) {
		if u := s.authenticateFabric(w, raw); u != nil {
			return u
		}
		return nil
	}
	_, u, err := s.st.Session(raw, s.cfg.SessionTTL)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid or expired session")
		return nil
	}
	return u
}

// authenticateFabric validates an estate-SSO token and returns its shadow user.
// user_id comes from the validated token ONLY — never a header/body/path — and
// is the sole storage-isolation key for that caller.
func (s *Server) authenticateFabric(w http.ResponseWriter, token string) *store.User {
	if s.ring == nil {
		writeErr(w, http.StatusUnauthorized, "fabric auth not configured")
		return nil
	}
	uid, err := s.ring.UserIDFor("Bearer "+token, s.now())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid or expired token")
		return nil
	}
	u, err := s.st.GetOrCreateFabricUser(uid)
	if err != nil {
		if errors.Is(err, store.ErrDisabled) {
			writeErr(w, http.StatusForbidden, "account disabled")
			return nil
		}
		writeErr(w, http.StatusInternalServerError, "identity error")
		return nil
	}
	return u
}

func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r)
		if !u.IsAdmin {
			writeErr(w, http.StatusForbidden, "admin only")
			return
		}
		next(w, r)
	})
}

type rateBucket struct {
	failures int
	window   time.Time
}

func (s *Server) loginAllowed(key string) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	b, ok := s.rate[key]
	if !ok {
		return true
	}
	if time.Since(b.window) > 15*time.Minute {
		delete(s.rate, key)
		return true
	}
	return b.failures < 10
}

func (s *Server) loginFailed(key string) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	b, ok := s.rate[key]
	if !ok || time.Since(b.window) > 15*time.Minute {
		s.rate[key] = &rateBucket{failures: 1, window: time.Now()}
		return
	}
	b.failures++
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v %s%s: %v", r.Method, r.URL.Path, "", rec)
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		q := ""
		if !strings.HasPrefix(r.URL.Path, "/s/") { // never log share tokens
			q = r.URL.RawQuery
		}
		u := UserFrom(r)
		uname := "-"
		if u != nil {
			uname = u.Username
		}
		log.Printf("%s %s%s %d %s %s", r.Method, r.URL.Path, q, sw.code, uname, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) { w.code = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	return w.ResponseWriter.Write(b)
}

// Flush passthrough for streamed downloads/uploads.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---- json helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(dst)
}

func mapFsErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fsdrv.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, fsdrv.ErrExists):
		writeErr(w, http.StatusConflict, "target already exists")
	case errors.Is(err, fsdrv.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid path or name")
	case errors.Is(err, fsdrv.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "file exceeds upload limit")
	case errors.Is(err, fsdrv.ErrConflict):
		writeErr(w, http.StatusPreconditionFailed, "etag mismatch: file changed elsewhere")
	default:
		log.Printf("internal error: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}
