package api

import (
	"archive/zip"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"xxdrive/internal/fsdrv"
	"xxdrive/internal/store"
)

func newToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashPassword(password string) []byte { return store.HashPassword(password) }

// fakeFileInfo adapts a version blob into os.FileInfo for ServeContent.
type fakeFileInfo struct {
	name string
	size int64
}

func (f *fakeFileInfo) Name() string       { return f.name }
func (f *fakeFileInfo) Size() int64        { return f.size }
func (f *fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f *fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f *fakeFileInfo) IsDir() bool        { return false }
func (f *fakeFileInfo) Sys() any           { return nil }

// ---- auth handlers ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ip := clientIP(r)
	key := ip + "|" + req.Username
	if !s.loginAllowed(key) {
		writeErr(w, http.StatusTooManyRequests, "too many failed logins; try later")
		return
	}
	u, err := s.st.UserByName(req.Username)
	if err != nil || u.Disabled || !store.CheckPassword(u.PasswordHash, req.Password) {
		s.loginFailed(key)
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	tok := newToken(32)
	label := "web"
	if r.Header.Get("X-Client") == "cli" {
		label = "cli:" + ip
	}
	if err := s.st.CreateSession(u.ID, tok, label, s.cfg.SessionTTL); err != nil {
		writeErr(w, 500, "session error")
		return
	}
	s.st.AddEvent(u.ID, "login", "from "+ip)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(s.cfg.SessionTTL.Seconds()),
		Secure: s.cfg.TLSCert != "",
	})
	writeJSON(w, 200, map[string]any{"token": tok, "user": map[string]any{
		"username": u.Username, "isAdmin": u.IsAdmin,
	}})
}

// handleFabricLogin is the estate-SSO entry for the browser: exchange a
// ClusterKeyring v1 token (minted by xx-chat's POST /api/v1/fabric/login) for a
// normal xx-drive session cookie backed by the same estate identity. The token
// may arrive in the JSON body {"token": "v1..."} or as Authorization: Bearer.
// No password is ever seen here — xx-chat is the account/password authority;
// this node only validates the token locally against the shared keyring.
func (s *Server) handleFabricLogin(w http.ResponseWriter, r *http.Request) {
	if s.ring == nil {
		writeErr(w, http.StatusServiceUnavailable, "fabric auth not configured")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		req.Token = strings.TrimPrefix(h, "Bearer ")
	} else if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" {
		writeErr(w, http.StatusBadRequest, "missing token")
		return
	}
	uid, err := s.ring.UserIDFor("Bearer "+req.Token, s.now())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}
	u, err := s.st.GetOrCreateFabricUser(uid)
	if err != nil {
		if err == store.ErrDisabled {
			writeErr(w, http.StatusForbidden, "account disabled")
			return
		}
		writeErr(w, http.StatusInternalServerError, "identity error")
		return
	}
	tok := newToken(32)
	if err := s.st.CreateSession(u.ID, tok, "fabric-sso", s.cfg.SessionTTL); err != nil {
		writeErr(w, 500, "session error")
		return
	}
	s.st.AddEvent(u.ID, "login", "fabric SSO from "+clientIP(r))
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(s.cfg.SessionTTL.Seconds()),
		Secure: s.cfg.TLSCert != "",
	})
	writeJSON(w, 200, map[string]any{"token": tok, "user": map[string]any{
		"username": u.Username, "isAdmin": u.IsAdmin, "fabric": true,
	}})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.st.DeleteSession(store.HashToken(c.Value))
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		_ = s.st.DeleteSession(store.HashToken(strings.TrimPrefix(h, "Bearer ")))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	writeJSON(w, 200, map[string]any{"username": u.Username, "isAdmin": u.IsAdmin, "fabric": u.FabricID != ""})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	sess, err := s.st.ListSessions(u.ID)
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	type row struct {
		Label     string `json:"label"`
		CreatedAt int64  `json:"createdAt"`
		LastSeen  int64  `json:"lastSeen"`
		ExpiresAt int64  `json:"expiresAt"`
		Current   bool   `json:"current"`
	}
	curHash := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		curHash = store.HashToken(c.Value)
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		curHash = store.HashToken(strings.TrimPrefix(h, "Bearer "))
	}
	out := make([]row, 0, len(sess))
	for _, x := range sess {
		out = append(out, row{Label: x.Label, CreatedAt: x.CreatedAt, LastSeen: x.LastSeen, ExpiresAt: x.ExpiresAt, Current: x.TokenHash == curHash})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleRevokeOthers(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	curHash := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		curHash = store.HashToken(c.Value)
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		curHash = store.HashToken(strings.TrimPrefix(h, "Bearer "))
	}
	sess, _ := s.st.ListSessions(u.ID)
	n := 0
	for _, x := range sess {
		if x.TokenHash != curHash {
			if s.st.DeleteSession(x.TokenHash) == nil {
				n++
			}
		}
	}
	s.st.AddEvent(u.ID, "revoke_sessions", fmt.Sprintf("%d revoked", n))
	writeJSON(w, 200, map[string]int{"revoked": n})
}

// ---- files ----

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	path := r.URL.Query().Get("path")
	ents, err := s.fs.List(u.Username, path)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	stars, _ := s.st.StarredPaths(u.ID)
	for _, e := range ents {
		e.Starred = stars[e.Path]
	}
	dirPath, err := fsdrv.ValidateRel(path)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"path": dirPath, "entries": ents})
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct{ Path string }
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	if err := s.fs.Mkdir(u.Username, req.Path); err != nil {
		mapFsErr(w, err)
		return
	}
	logical, _ := fsdrv.ValidateRel(req.Path)
	s.st.AddEvent(u.ID, "mkdir", logical)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleUpload streams multipart "file" part to path (?path=/dir/name.ext).
// Optional query conflict=rename → write conflict copy instead of overwrite.
// Optional If-Match header must equal current weak etag to allow overwrite.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	target := r.URL.Query().Get("path")
	conflictRename := r.URL.Query().Get("conflict") == "rename"
	deviceTag := r.Header.Get("X-Device")
	// Cap the whole request body up front (multipart overhead allowance),
	// before any part is parsed.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadMB<<20+(10<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, "expected multipart form")
		return
	}
	var (
		found      bool
		shaHex     string
		entry      *fsdrv.Entry
		conflicted bool
		prev       fsdrv.VersionInfo
	)
	maxBytes := s.cfg.MaxUploadMB << 20
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			mapFsErr(w, perr)
			return
		}
		if part.FormName() != "file" {
			continue
		}
		found = true
		shaHex, entry, conflicted, prev, err = s.fs.Upload(u.Username, target, part, maxBytes,
			r.Header.Get("If-Match"), conflictRename, deviceTag)
		break
	}
	if !found {
		writeErr(w, 400, "missing file field")
		return
	}
	if err != nil {
		mapFsErr(w, err)
		return
	}
	s.st.PutEtag(u.ID, entry.Path, entry.Size, entry.ModTime, shaHex)
	if prev.VersionID != "" {
		// index the snapshotted previous content under the originally requested path
		s.st.AddVersion(u.ID, mustValidate(target), prev.VersionID, prev.Size)
	}
	s.st.AddEvent(u.ID, "upload", entry.Path+fmt.Sprintf(" (%d bytes)", entry.Size))
	status := http.StatusOK
	if conflicted {
		status = http.StatusCreated
	}
	w.Header().Set("ETag", `"`+shaHex[:16]+`"`)
	writeJSON(w, status, map[string]any{"entry": entry, "sha256": shaHex, "conflicted": conflicted})
}

func (s *Server) serveFileDownload(w http.ResponseWriter, r *http.Request, openF func() (*os.File, os.FileInfo, error), name string, inline bool) {
	f, fi, err := openF()
	if err != nil {
		mapFsErr(w, err)
		return
	}
	defer f.Close()
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename*=UTF-8''%s`, disposition, urlPathEscape(name)))
	w.Header().Set("ETag", fsdrv.EtagOf(fi.ModTime().Unix(), fi.Size()))
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	q := r.URL.Query()
	path := q.Get("path")
	inline := q.Get("inline") == "1"
	name := fsdrv.NameOf(mustValidate(path))
	if name == "" {
		writeErr(w, 400, "cannot download root")
		return
	}
	s.serveFileDownload(w, r, func() (*os.File, os.FileInfo, error) {
		f, fi, _, err := s.fs.OpenFile(u.Username, path)
		return f, fi, err
	}, name, inline)
	// etag cache fill (best effort, files < 512MB only)
	if logical := mustValidate(path); logical != "/" {
		if fi, err := s.fs.Stat(u.Username, path); err == nil && !fi.IsDir && fi.Size < 512<<20 {
			if _, ok := s.st.CachedEtag(u.ID, logical, fi.Size, fi.ModTime); !ok {
				if shaHex, ferr := s.fs.HashFile(u.Username, path); ferr == nil {
					s.st.PutEtag(u.ID, logical, fi.Size, fi.ModTime, shaHex)
				}
			}
		}
	}
}

func (s *Server) handleZip(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	path := r.URL.Query().Get("path")
	logical, err := fsdrv.ValidateRel(path)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	name := fsdrv.NameOf(logical)
	if name == "" {
		name = "drive"
	}
	zw := zip.NewWriter(w)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename*=UTF-8''%s.zip`, urlPathEscape(name)))
	baseInZip := ""
	if name != "drive" {
		baseInZip = name + "/"
	}
	err = s.fs.WalkTree(u.Username, logical, func(e *fsdrv.Entry, abs string) error {
		relInZip := baseInZip + strings.TrimPrefix(strings.TrimPrefix(e.Path, logical), "/")
		if e.IsDir {
			_, err := zw.CreateHeader(&zip.FileHeader{Name: relInZip + "/", Modified: time.Unix(e.ModTime, 0)})
			return err
		}
		hdr := &zip.FileHeader{Name: relInZip, Method: zip.Deflate, Modified: time.Unix(e.ModTime, 0)}
		hdr.SetMode(0o644)
		fw, cerr := zw.CreateHeader(hdr)
		if cerr != nil {
			return cerr
		}
		f, ferr := os.Open(abs)
		if ferr != nil {
			return nil // skip unreadable
		}
		defer f.Close()
		_, cerr = io.Copy(fw, f)
		return cerr
	})
	if err != nil {
		// headers already sent; nothing to do but abort the stream
		return
	}
	zw.Close()
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct {
		Path    string `json:"path"`
		NewName string `json:"newName"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	entry, err := s.fs.Rename(u.Username, req.Path, req.NewName)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	oldLogical, _ := fsdrv.ValidateRel(req.Path)
	s.st.MoveEtags(u.ID, oldLogical, entry.Path)
	s.st.AddEvent(u.ID, "rename", oldLogical+" → "+entry.Path)
	writeJSON(w, 200, map[string]any{"entry": entry})
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct {
		Path    string `json:"path"`
		DestDir string `json:"destDir"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	entry, err := s.fs.Move(u.Username, req.Path, req.DestDir)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	oldLogical, _ := fsdrv.ValidateRel(req.Path)
	s.st.MoveEtags(u.ID, oldLogical, entry.Path)
	s.st.AddEvent(u.ID, "move", oldLogical+" → "+entry.Path)
	writeJSON(w, 200, map[string]any{"entry": entry})
}

func (s *Server) handleCopy(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct {
		Path    string `json:"path"`
		DestDir string `json:"destDir"`
		NewName string `json:"newName"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	entry, err := s.fs.Copy(u.Username, req.Path, req.DestDir, req.NewName)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	s.st.AddEvent(u.ID, "copy", req.Path+" → "+entry.Path)
	writeJSON(w, 200, map[string]any{"entry": entry})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct{ Path string }
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	id, orig, err := s.fs.Delete(u.Username, req.Path)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	s.st.DeleteEtagsUnder(u.ID, orig)
	s.st.DeleteVersionsForPath(u.ID, orig)
	s.st.AddEvent(u.ID, "delete", orig)
	writeJSON(w, 200, map[string]string{"trashId": id})
}

// ---- trash ----

func (s *Server) handleTrashList(w http.ResponseWriter, r *http.Request) {
	items, err := s.fs.ListTrash(UserFrom(r).Username)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	writeJSON(w, 200, items)
}

func (s *Server) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct{ ID string }
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	dest, err := s.fs.Restore(u.Username, req.ID)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	s.st.AddEvent(u.ID, "restore", dest)
	writeJSON(w, 200, map[string]string{"restoredTo": dest})
}

func (s *Server) handleTrashDelete(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct{ ID string }
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	if err := s.fs.PurgeTrashItem(u.Username, req.ID); err != nil {
		mapFsErr(w, err)
		return
	}
	s.st.AddEvent(u.ID, "purge", req.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleTrashEmpty(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	if err := s.fs.EmptyTrash(u.Username); err != nil {
		mapFsErr(w, err)
		return
	}
	s.st.AddEvent(u.ID, "empty_trash", "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- versions ----

func (s *Server) handleVersionList(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	path := r.URL.Query().Get("path")
	vs, err := s.st.ListVersions(u.ID, mustValidate(path))
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	writeJSON(w, 200, vs)
}

func (s *Server) handleVersionRestore(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct {
		Path      string `json:"path"`
		VersionID string `json:"versionId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	if _, err := s.st.GetVersion(u.ID, mustValidate(req.Path), req.VersionID); err != nil {
		writeErr(w, 404, "version not found")
		return
	}
	// snapshot current as a version first (restores are reversible)
	if vid, size, err := s.fs.SnapshotCurrent(u.Username, req.Path); err == nil {
		s.st.AddVersion(u.ID, mustValidate(req.Path), vid, size)
	}
	if err := s.fs.RestoreVersion(u.Username, req.Path, req.VersionID); err != nil {
		mapFsErr(w, err)
		return
	}
	s.st.AddEvent(u.ID, "version_restore", req.Path+" @ "+req.VersionID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleVersionDownload(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	q := r.URL.Query()
	path := q.Get("path")
	vid := q.Get("versionId")
	if len(vid) < 8 {
		writeErr(w, 400, "bad version id")
		return
	}
	name := fsdrv.NameOf(mustValidate(path)) + "." + vid[:8]
	s.serveFileDownload(w, r, func() (*os.File, os.FileInfo, error) {
		f, size, err := s.fs.OpenVersion(u.Username, path, vid)
		if err != nil {
			return nil, nil, err
		}
		fi := &fakeFileInfo{name: name, size: size}
		return f, fi, nil
	}, name, false)
}

// ---- search / stars / events ----

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, 200, []*fsdrv.Entry{})
		return
	}
	ents, err := s.fs.Search(u.Username, q, 200)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	stars, _ := s.st.StarredPaths(u.ID)
	for _, e := range ents {
		e.Starred = stars[e.Path]
	}
	writeJSON(w, 200, ents)
}

func (s *Server) handleStarToggle(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct{ Path string }
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	p := mustValidate(req.Path)
	on, err := s.st.ToggleStar(u.ID, p)
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	writeJSON(w, 200, map[string]bool{"starred": on})
}

func (s *Server) handleStarred(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	stars, err := s.st.StarredPaths(u.ID)
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	out := []*fsdrv.Entry{}
	for p := range stars {
		if e, err := s.fs.Stat(u.Username, p); err == nil {
			e.Starred = true
			out = append(out, e)
		} else {
			s.st.ToggleStar(u.ID, p) // prune dead star
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	evs, err := s.st.ListEvents(UserFrom(r).ID, limit)
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	writeJSON(w, 200, evs)
}

// ---- admin ----

type adminUserRow struct {
	Username  string `json:"username"`
	IsAdmin   bool   `json:"isAdmin"`
	Disabled  bool   `json:"disabled"`
	CreatedAt int64  `json:"createdAt"`
}

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.st.ListUsers()
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	out := make([]adminUserRow, 0, len(users))
	for _, u := range users {
		out = append(out, adminUserRow{u.Username, u.IsAdmin, u.Disabled, u.CreatedAt})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"isAdmin"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, 400, "password must be at least 8 characters")
		return
	}
	u, err := s.st.CreateUser(req.Username, hashPassword(req.Password), req.IsAdmin)
	if err != nil {
		if err == store.ErrConflict {
			writeErr(w, http.StatusConflict, "user exists")
			return
		}
		writeErr(w, 400, "invalid username")
		return
	}
	s.st.AddEvent(u.ID, "admin_create_user", req.Username)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleAdminSetState(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Disabled bool   `json:"disabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	target, err := s.st.UserByName(req.Username)
	if err != nil {
		writeErr(w, 404, "no such user")
		return
	}
	if target.ID == UserFrom(r).ID {
		writeErr(w, 400, "cannot disable yourself")
		return
	}
	if err := s.st.SetDisabled(target.ID, req.Disabled); err != nil {
		writeErr(w, 500, "error")
		return
	}
	s.st.AddEvent(UserFrom(r).ID, "admin_set_state", req.Username)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleAdminSetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, 400, "password must be at least 8 characters")
		return
	}
	target, err := s.st.UserByName(req.Username)
	if err != nil {
		writeErr(w, 404, "no such user")
		return
	}
	if err := s.st.SetPassword(target.ID, hashPassword(req.Password)); err != nil {
		writeErr(w, 500, "error")
		return
	}
	s.st.AddEvent(UserFrom(r).ID, "admin_reset_password", req.Username)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- small helpers ----

// ---- small helpers ----

func mustValidate(p string) string {
	v, err := fsdrv.ValidateRel(p)
	if err != nil {
		return "/"
	}
	return v
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func urlPathEscape(name string) string { return url.PathEscape(name) }
