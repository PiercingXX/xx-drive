// Package store provides the SQLite-backed metadata layer for xx-drive:
// users, sessions, share links, stars, events, version index and etag cache.
//
// Design notes:
//   - File contents live on the plain filesystem (see package fsdrv); this DB
//     is metadata only and every table except users/sessions is rebuildable.
//   - Tokens (session + share) are stored as SHA-256 hashes; raw tokens only
//     ever exist in cookies/links handed to clients.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"

	_ "modernc.org/sqlite"
)

// Password hashing: PBKDF2-HMAC-SHA256, 600k iterations (OWASP 2023 guidance),
// 16-byte random salt, stored as "pbkdf2-sha256$<iter>$<salt>$<dk>".
const pwIterations = 600000

func HashPassword(password string) []byte {
	salt := make([]byte, 16)
	// crypto/rand failing means the OS entropy source is broken. The signature
	// cannot return an error without breaking callers in other packages, and
	// silently proceeding with a predictable salt would weaken every stored
	// password hash — so fail loudly and deliberately instead.
	if _, err := rand.Read(salt); err != nil {
		panic(fmt.Sprintf("xxdrive/store: crypto/rand failed generating password salt: %v", err))
	}
	dk := pbkdf2.Key([]byte(password), salt, pwIterations, 32, sha256.New)
	return []byte(fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pwIterations, hex.EncodeToString(salt), hex.EncodeToString(dk)))
}

func CheckPassword(stored []byte, password string) bool {
	parts := strings.Split(string(stored), "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	var iter int
	if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	salt, err1 := hex.DecodeString(parts[2])
	want, err2 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2.Key([]byte(password), salt, iter, len(want), sha256.New)
	return subtle.ConstantTimeCompare(got, want) == 1
}

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
	ErrDisabled = errors.New("account disabled")
)

type User struct {
	ID           int64
	Username     string
	PasswordHash []byte
	IsAdmin      bool
	Disabled     bool
	CreatedAt    int64
	// FabricID is the estate account id (xx-chat users.id) for a user that
	// authenticates through estate SSO; empty for a local password user.
	// Populated only by GetOrCreateFabricUser / UserByFabricID.
	FabricID string
}

type Session struct {
	TokenHash string
	UserID    int64
	Label     string // e.g. "web", "cli:<host>"
	CreatedAt int64
	LastSeen  int64
	ExpiresAt int64
}

type Share struct {
	ID            int64
	TokenHash     string
	UserID        int64
	Path          string // path within owner's drive, always starting with "/"
	HasPassword   bool
	PasswordHash  []byte
	AllowDownload bool
	ExpiresAt     int64 // 0 = never
	CreatedAt     int64
	Revoked       bool
}

type Event struct {
	ID        int64
	UserID    int64
	Kind      string
	Detail    string
	CreatedAt int64
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// modernc/sqlite is happiest with limited write concurrency.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			username      TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			is_admin      INTEGER NOT NULL DEFAULT 0,
			disabled      INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL,
			fabric_id     TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY,
			user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			label      TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			last_seen  INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
		`CREATE TABLE IF NOT EXISTS shares (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			token_hash     TEXT NOT NULL UNIQUE,
			user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			path           TEXT NOT NULL,
			has_password   INTEGER NOT NULL DEFAULT 0,
			password_hash  TEXT NOT NULL DEFAULT '',
			allow_download INTEGER NOT NULL DEFAULT 1,
			expires_at     INTEGER NOT NULL DEFAULT 0,
			created_at     INTEGER NOT NULL,
			revoked        INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_shares_user ON shares(user_id)`,
		`CREATE TABLE IF NOT EXISTS stars (
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			path    TEXT NOT NULL,
			PRIMARY KEY (user_id, path)
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			kind       TEXT NOT NULL,
			detail     TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_user ON events(user_id, id DESC)`,
		`CREATE TABLE IF NOT EXISTS versions (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			path       TEXT NOT NULL,
			version_id TEXT NOT NULL,
			size       INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_versions_path ON versions(user_id, path)`,
		`CREATE TABLE IF NOT EXISTS etag_cache (
			user_id INTEGER NOT NULL,
			path    TEXT NOT NULL,
			size    INTEGER NOT NULL,
			mtime   INTEGER NOT NULL,
			sha256  TEXT NOT NULL,
			PRIMARY KEY (user_id, path)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// Additive migration for DBs created before estate-SSO: give existing
	// users tables the fabric_id column. Idempotent — a "duplicate column"
	// error just means a fresh DB already has it from the CREATE above.
	if _, err := s.db.Exec(`ALTER TABLE users ADD COLUMN fabric_id TEXT`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("migrate fabric_id: %w", err)
	}
	// A partial unique index: at most one user row per estate identity, while
	// local (password) users keep fabric_id NULL and stay unconstrained.
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_fabric ON users(fabric_id) WHERE fabric_id IS NOT NULL`); err != nil {
		return fmt.Errorf("migrate fabric idx: %w", err)
	}
	return nil
}

func HashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// ---- users ----

// validSegmentName reports whether name is safe to use as a single filesystem
// path segment under files/<name>: non-empty, within maxLen, free of path
// separators and NUL, not "." or "..", and in canonical form (filepath.Clean
// must not rewrite it). This is the single choke point guarding per-user
// storage isolation; every username and fabric id must pass through it before
// it can ever become a directory name on disk.
func validSegmentName(name string, maxLen int) bool {
	if name == "" || len(name) > maxLen {
		return false
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	return filepath.Clean(name) == name
}

// validUsername enforces the local-username rules (1-64 chars, one clean
// segment). Both admin creation and first-boot bootstrap go through CreateUser,
// so this covers every path a username can enter the system.
func validUsername(name string) bool { return validSegmentName(name, 64) }

func (s *Store) CreateUser(username string, passwordHash []byte, isAdmin bool) (*User, error) {
	if !validUsername(username) {
		return nil, fmt.Errorf("%w: invalid username", ErrNotFound)
	}
	res, err := s.db.Exec(`INSERT INTO users (username, password_hash, is_admin, created_at) VALUES (?,?,?,?)`,
		username, string(passwordHash), b2i(isAdmin), time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrConflict
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, PasswordHash: passwordHash, IsAdmin: isAdmin, CreatedAt: time.Now().Unix()}, nil
}

func (s *Store) UserByName(username string) (*User, error) {
	row := s.db.QueryRow(`SELECT id, username, password_hash, is_admin, disabled, created_at, fabric_id FROM users WHERE username = ?`, username)
	return scanUser(row)
}

func (s *Store) UserByID(id int64) (*User, error) {
	row := s.db.QueryRow(`SELECT id, username, password_hash, is_admin, disabled, created_at, fabric_id FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// fabricShadowUsername is the storage-isolation key (and files/<name> directory)
// for an estate-SSO user. It is derived ONLY from the validated token's user_id,
// namespaced with a prefix so it can never collide with a human-chosen local
// username and is self-documenting on disk.
func fabricShadowUsername(fabricID string) string { return "fabric_" + fabricID }

// GetOrCreateFabricUser resolves an estate account id (from a validated fabric
// token) to the local shadow user that owns that identity's drive, creating it
// on first sight. The returned user's Username is the per-user storage key used
// everywhere downstream (files/<username>, and the integer ID for metadata
// tables). The shadow user has an unusable password hash — it can never log in
// via the local password path; the only way to become this user is to present a
// valid fabric token for the same user_id.
func (s *Store) GetOrCreateFabricUser(fabricID string) (*User, error) {
	// Validate BEFORE prefixing: the shadow username "fabric_"+id becomes a
	// directory name under files/, so the raw id must itself be one clean
	// path segment.
	if !validSegmentName(fabricID, 200) {
		return nil, fmt.Errorf("%w: invalid fabric id", ErrNotFound)
	}
	if u, err := s.UserByFabricID(fabricID); err == nil {
		if u.Disabled {
			return nil, ErrDisabled
		}
		return u, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	// First sight: create the shadow row. "!" is a hash that can never match
	// CheckPassword (wrong scheme prefix), so password login is impossible.
	uname := fabricShadowUsername(fabricID)
	_, err := s.db.Exec(`INSERT INTO users (username, password_hash, is_admin, created_at, fabric_id) VALUES (?,?,?,?,?)`,
		uname, "!", 0, time.Now().Unix(), fabricID)
	if err != nil {
		// Lost a create race, or username already taken — fall back to lookup.
		if u, lerr := s.UserByFabricID(fabricID); lerr == nil {
			if u.Disabled {
				return nil, ErrDisabled
			}
			return u, nil
		}
		return nil, err
	}
	return s.UserByFabricID(fabricID)
}

// UserByFabricID looks up the shadow user for an estate identity.
func (s *Store) UserByFabricID(fabricID string) (*User, error) {
	row := s.db.QueryRow(`SELECT id, username, password_hash, is_admin, disabled, created_at, fabric_id FROM users WHERE fabric_id = ?`, fabricID)
	var u User
	var isAdmin, disabled int
	var fid sql.NullString
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &isAdmin, &disabled, &u.CreatedAt, &fid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	u.Disabled = disabled == 1
	u.FabricID = fid.String
	return &u, nil
}

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var isAdmin, disabled int
	var fid sql.NullString
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &isAdmin, &disabled, &u.CreatedAt, &fid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	u.Disabled = disabled == 1
	u.FabricID = fid.String
	return &u, nil
}

func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT id, username, password_hash, is_admin, disabled, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u := &User{}
		var isAdmin, disabled int
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &isAdmin, &disabled, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.IsAdmin = isAdmin == 1
		u.Disabled = disabled == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// DeleteUser removes the user row. Sessions/shares/stars/events/versions
// cascade; etag_cache has no FK so it is deleted explicitly. Caller must
// already have removed on-disk files for this username.
func (s *Store) DeleteUser(userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM etag_cache WHERE user_id = ?`, userID); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) SetPassword(userID int64, hash []byte) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), userID)
	return err
}

func (s *Store) SetDisabled(userID int64, disabled bool) error {
	_, err := s.db.Exec(`UPDATE users SET disabled = ? WHERE id = ?`, b2i(disabled), userID)
	if err == nil && disabled {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	}
	return err
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// CheckSharePassword verifies a candidate password against a share.
func (s *Store) CheckSharePassword(sh *Share, candidate string) bool {
	if !sh.HasPassword {
		return true
	}
	return CheckPassword(sh.PasswordHash, candidate)
}

// ---- sessions ----

func (s *Store) CreateSession(userID int64, token string, label string, ttl time.Duration) error {
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, label, created_at, last_seen, expires_at) VALUES (?,?,?,?,?,?)`,
		HashToken(token), userID, label, now.Unix(), now.Unix(), now.Add(ttl).Unix())
	return err
}

// Session validates a raw token, sliding its expiry forward.
func (s *Store) Session(token string, slidingTTL time.Duration) (*Session, *User, error) {
	th := HashToken(token)
	row := s.db.QueryRow(`SELECT token_hash, user_id, label, created_at, last_seen, expires_at FROM sessions WHERE token_hash = ?`, th)
	var sess Session
	err := row.Scan(&sess.TokenHash, &sess.UserID, &sess.Label, &sess.CreatedAt, &sess.LastSeen, &sess.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().Unix()
	if now >= sess.ExpiresAt {
		_ = s.DeleteSession(th)
		return nil, nil, ErrNotFound
	}
	u, err := s.UserByID(sess.UserID)
	if err != nil {
		return nil, nil, err
	}
	if u.Disabled {
		return nil, nil, ErrDisabled
	}
	newExp := now + int64(slidingTTL.Seconds())
	if newExp > sess.ExpiresAt+int64(slidingTTL.Seconds()/2) { // throttle writes
		_, _ = s.db.Exec(`UPDATE sessions SET last_seen = ?, expires_at = ? WHERE token_hash = ?`, now, newExp, th)
	}
	return &sess, u, nil
}

func (s *Store) ListSessions(userID int64) ([]*Session, error) {
	rows, err := s.db.Query(`SELECT token_hash, user_id, label, created_at, last_seen, expires_at FROM sessions WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.TokenHash, &sess.UserID, &sess.Label, &sess.CreatedAt, &sess.LastSeen, &sess.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, &sess)
	}
	return out, rows.Err()
}

// ---- shares ----

func (s *Store) CreateShare(userID int64, path, token string, passwordHash []byte, allowDownload bool, expiresAt int64) (*Share, error) {
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO shares (token_hash, user_id, path, has_password, password_hash, allow_download, expires_at, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		HashToken(token), userID, path, b2i(len(passwordHash) > 0), string(passwordHash), b2i(allowDownload), expiresAt, now.Unix())
	if err != nil {
		return nil, err
	}
	return &Share{TokenHash: HashToken(token), UserID: userID, Path: path,
		HasPassword: len(passwordHash) > 0, PasswordHash: passwordHash,
		AllowDownload: allowDownload, ExpiresAt: expiresAt, CreatedAt: now.Unix()}, nil
}

// ShareByToken resolves a raw share token to a live (non-revoked, non-expired) share.
func (s *Store) ShareByToken(token string) (*Share, error) {
	row := s.db.QueryRow(`SELECT id, token_hash, user_id, path, has_password, password_hash, allow_download, expires_at, created_at, revoked FROM shares WHERE token_hash = ?`, HashToken(token))
	sh := &Share{}
	var hasPw, allowDl, revoked int
	err := row.Scan(&sh.ID, &sh.TokenHash, &sh.UserID, &sh.Path, &hasPw, &sh.PasswordHash, &allowDl, &sh.ExpiresAt, &sh.CreatedAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sh.HasPassword = hasPw == 1
	sh.AllowDownload = allowDl == 1
	sh.Revoked = revoked == 1
	if sh.Revoked || (sh.ExpiresAt != 0 && time.Now().Unix() >= sh.ExpiresAt) {
		return nil, ErrNotFound
	}
	return sh, nil
}

func (s *Store) ListShares(userID int64) ([]*Share, error) {
	rows, err := s.db.Query(`SELECT id, token_hash, user_id, path, has_password, password_hash, allow_download, expires_at, created_at, revoked FROM shares WHERE user_id = ? AND revoked = 0 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Share
	for rows.Next() {
		sh := &Share{}
		var hasPw, allowDl, revoked int
		if err := rows.Scan(&sh.ID, &sh.TokenHash, &sh.UserID, &sh.Path, &hasPw, &sh.PasswordHash, &allowDl, &sh.ExpiresAt, &sh.CreatedAt, &revoked); err != nil {
			return nil, err
		}
		sh.HasPassword = hasPw == 1
		sh.AllowDownload = allowDl == 1
		sh.Revoked = revoked == 1
		out = append(out, sh)
	}
	return out, rows.Err()
}

func (s *Store) RevokeShare(userID int64, tokenHash string) error {
	res, err := s.db.Exec(`UPDATE shares SET revoked = 1 WHERE user_id = ? AND token_hash = ?`, userID, tokenHash)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- stars ----

func (s *Store) ToggleStar(userID int64, path string) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM stars WHERE user_id = ? AND path = ?`, userID, path).Scan(&exists)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(`INSERT INTO stars (user_id, path) VALUES (?,?)`, userID, path)
		return true, err
	case err != nil:
		return false, err
	default:
		_, err = s.db.Exec(`DELETE FROM stars WHERE user_id = ? AND path = ?`, userID, path)
		return false, err
	}
}

func (s *Store) StarredPaths(userID int64) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT path FROM stars WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// ---- events ----

func (s *Store) AddEvent(userID int64, kind, detail string) {
	_, _ = s.db.Exec(`INSERT INTO events (user_id, kind, detail, created_at) VALUES (?,?,?,?)`,
		userID, kind, detail, time.Now().Unix())
}

func (s *Store) ListEvents(userID int64, limit int) ([]*Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, user_id, kind, detail, created_at FROM events WHERE user_id = ? ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		e := &Event{}
		if err := rows.Scan(&e.ID, &e.UserID, &e.Kind, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- versions ----

func (s *Store) AddVersion(userID int64, path, versionID string, size int64) error {
	_, err := s.db.Exec(`INSERT INTO versions (user_id, path, version_id, size, created_at) VALUES (?,?,?,?,?)`,
		userID, path, versionID, size, time.Now().Unix())
	return err
}

type VersionInfo struct {
	VersionID string `json:"versionId"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"createdAt"`
}

func (s *Store) ListVersions(userID int64, path string) ([]VersionInfo, error) {
	rows, err := s.db.Query(`SELECT version_id, size, created_at FROM versions WHERE user_id = ? AND path = ? ORDER BY created_at DESC`, userID, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VersionInfo
	for rows.Next() {
		var v VersionInfo
		if err := rows.Scan(&v.VersionID, &v.Size, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) GetVersion(userID int64, path, versionID string) (*VersionInfo, error) {
	var v VersionInfo
	err := s.db.QueryRow(`SELECT version_id, size, created_at FROM versions WHERE user_id = ? AND path = ? AND version_id = ?`,
		userID, path, versionID).Scan(&v.VersionID, &v.Size, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &v, err
}

// DeleteVersionsForPath drops version rows for exactly one path.
// Prefer DeleteVersionsUnder when a subtree (directory) is being purged.
func (s *Store) DeleteVersionsForPath(userID int64, path string) {
	_, _ = s.db.Exec(`DELETE FROM versions WHERE user_id = ? AND path = ?`, userID, path)
}

// RelocateVersions rewrites version-index rows after a file or directory
// moved from oldPath to newPath (every descendant of oldPath included).
// It returns the distinct OLD logical paths that had rows, so the caller can
// rename the matching on-disk blob directories under
// versions/<user>/<pathHash(logical)> in lockstep. Rows are updated in one
// transaction; a path with no rows relocates nothing and is not an error.
func (s *Store) RelocateVersions(userID int64, oldPath, newPath string) ([]string, error) {
	if oldPath == "" || oldPath == "/" || newPath == "" || newPath == "/" {
		return nil, nil
	}
	var moved []string
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`SELECT DISTINCT path FROM versions WHERE user_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
		userID, oldPath, EscapeLike(oldPath)+"/%")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		moved = append(moved, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(moved) > 0 {
		// substr/length both operate on characters inside SQLite, so the
		// suffix rewrite is UTF-8-safe regardless of byte lengths.
		if _, err := tx.Exec(
			`UPDATE versions SET path = ? || substr(path, length(?)+1)
			 WHERE user_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
			newPath, oldPath, userID, oldPath, EscapeLike(oldPath)+"/%"); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return moved, nil
}

// VersionRef identifies one version-index row (path + random version id).
type VersionRef struct {
	Path      string
	VersionID string
}

// VersionsUnder lists the version rows for root and everything below it, so a
// caller can snapshot EXACTLY which rows belong to an item before an
// operation that might otherwise mix them with newer rows sharing the path.
func (s *Store) VersionsUnder(userID int64, root string) ([]VersionRef, error) {
	rows, err := s.db.Query(
		`SELECT path, version_id FROM versions WHERE user_id = ? AND (path = ? OR path LIKE ? ESCAPE '\') ORDER BY id`,
		userID, root, EscapeLike(root)+"/%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VersionRef
	for rows.Next() {
		var ref VersionRef
		if err := rows.Scan(&ref.Path, &ref.VersionID); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// MoveSpecificVersions atomically relocates exactly the listed version rows:
// each is deleted from its recorded path under oldRoot and re-inserted with
// identical size/created_at at the equivalent spot under newRoot. Rows NOT in
// the list — e.g. history a newer file accumulated at the same paths — are
// untouched; listed-but-missing rows are skipped. This is what keeps two
// files' histories separate when an older trash item restores beside its
// successor instead of merging every row under one path.
func (s *Store) MoveSpecificVersions(userID int64, refs []VersionRef, oldRoot, newRoot string) error {
	if len(refs) == 0 || oldRoot == "" || oldRoot == "/" || newRoot == "" || newRoot == "/" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, ref := range refs {
		if !strings.HasPrefix(ref.Path, oldRoot) {
			continue // outside the claimed subtree: never touch it
		}
		np := newRoot + strings.TrimPrefix(ref.Path, oldRoot)
		res, err := tx.Exec(`INSERT INTO versions (user_id, path, version_id, size, created_at)
			SELECT user_id, ?, version_id, size, created_at FROM versions
			WHERE user_id = ? AND path = ? AND version_id = ?`,
			np, userID, ref.Path, ref.VersionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // already gone
		}
		if _, err := tx.Exec(`DELETE FROM versions WHERE user_id = ? AND path = ? AND version_id = ?`,
			userID, ref.Path, ref.VersionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteSpecificVersions permanently drops exactly the given version rows
// (path + version id pairs) and returns the distinct paths that lost rows so
// the caller can remove the matching blob files. Used by permanent-purge
// sites when the trash metadata records which versions an item owned — a
// NEWER file recreated at the same logical path keeps its own history.
func (s *Store) DeleteSpecificVersions(userID int64, refs []VersionRef) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	seen := map[string]bool{}
	var removed []string
	for _, ref := range refs {
		res, err := tx.Exec(`DELETE FROM versions WHERE user_id = ? AND path = ? AND version_id = ?`,
			userID, ref.Path, ref.VersionID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 && !seen[ref.Path] {
			seen[ref.Path] = true
			removed = append(removed, ref.Path)
		}
	}
	return removed, tx.Commit()
}

// DeleteVersionsUnder permanently drops version rows for path and everything
// below it. It returns the distinct paths removed so the caller can delete
// the matching blob directories (permanent-purge sites only — trashing a
// file must KEEP its history).
func (s *Store) DeleteVersionsUnder(userID int64, prefix string) ([]string, error) {
	if prefix == "" || prefix == "/" {
		return nil, fmt.Errorf("refusing to delete all versions for user %d", userID)
	}
	var removed []string
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`SELECT DISTINCT path FROM versions WHERE user_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
		userID, prefix, EscapeLike(prefix)+"/%")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		removed = append(removed, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(removed) > 0 {
		if _, err := tx.Exec(`DELETE FROM versions WHERE user_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
			userID, prefix, EscapeLike(prefix)+"/%"); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return removed, nil
}

// PruneVersions keeps at most keep newest versions per (user,path).
func (s *Store) PruneVersions(keep int) (pruned []struct {
	Username  string
	Path      string
	VersionID string
}, err error) {
	rows, err := s.db.Query(`SELECT u.username, v.path, v.version_id FROM (
		SELECT id, user_id, path, version_id, ROW_NUMBER() OVER (PARTITION BY user_id, path ORDER BY created_at DESC, id DESC) AS rn
		FROM versions) v JOIN users u ON u.id = v.user_id WHERE v.rn > ?`, keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p struct {
			Username  string
			Path      string
			VersionID string
		}
		if err := rows.Scan(&p.Username, &p.Path, &p.VersionID); err != nil {
			return nil, err
		}
		pruned = append(pruned, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, p := range pruned {
		if _, err := s.db.Exec(`DELETE FROM versions WHERE version_id = ? AND path = ? AND user_id = (SELECT id FROM users WHERE username = ?)`,
			p.VersionID, p.Path, p.Username); err != nil {
			return pruned, err
		}
	}
	return pruned, nil
}

// ---- etag cache ----

func (s *Store) CachedEtag(userID int64, path string, size, mtime int64) (string, bool) {
	var sha string
	var sz, mt int64
	err := s.db.QueryRow(`SELECT size, mtime, sha256 FROM etag_cache WHERE user_id = ? AND path = ?`, userID, path).Scan(&sz, &mt, &sha)
	if err != nil || sz != size || mt != mtime {
		return "", false
	}
	return sha, true
}

func (s *Store) PutEtag(userID int64, path string, size, mtime int64, sha string) {
	_, _ = s.db.Exec(`INSERT INTO etag_cache (user_id, path, size, mtime, sha256) VALUES (?,?,?,?,?)
		ON CONFLICT(user_id, path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, sha256=excluded.sha256`,
		userID, path, size, mtime, sha)
}

func (s *Store) DropEtag(userID int64, path string) {
	_, _ = s.db.Exec(`DELETE FROM etag_cache WHERE user_id = ? AND path = ?`, userID, path)
}

// RenameEtags moves etag cache entries when a subtree moves; returns affected paths.
func (s *Store) MoveEtags(userID int64, oldPrefix, newPrefix string) {
	rows, err := s.db.Query(`SELECT path FROM etag_cache WHERE user_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
		userID, oldPrefix, EscapeLike(oldPrefix)+"/%")
	if err != nil {
		return
	}
	var paths []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			paths = append(paths, p)
		}
	}
	rows.Close()
	for _, p := range paths {
		np := newPrefix + strings.TrimPrefix(p, oldPrefix)
		_, _ = s.db.Exec(`UPDATE etag_cache SET path = ? WHERE user_id = ? AND path = ?`, np, userID, p)
	}
}

func (s *Store) DeleteEtagsUnder(userID int64, prefix string) {
	_, _ = s.db.Exec(`DELETE FROM etag_cache WHERE user_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
		userID, prefix, EscapeLike(prefix)+"/%")
}

func EscapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
