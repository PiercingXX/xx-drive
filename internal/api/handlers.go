package api

import (
	"archive/zip"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
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

func newToken(n int) (string, error) {
	b := make([]byte, n)
	// crypto/rand failing means the OS entropy source is broken: callers must
	// fail the request (500) rather than mint a predictable token.
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashPassword(password string) []byte { return store.HashPassword(password) }

// dummyHash equalizes login timing for nonexistent usernames. A login for a
// real user pays one PBKDF2 verification (~hundreds of ms at the configured
// cost); before this seam an unknown username returned near-instantly,
// letting attackers enumerate valid users by response time alone. Unknown
// users now burn one store.HashPassword-equivalent computation (same cost
// parameters, same generic 401). Tests swap this var to count invocations
// without paying the real cost.
var dummyHash = store.HashPassword

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
		if err != nil || u.Disabled {
			// Unknown usernames AND disabled accounts burn the same PBKDF2
			// cost a real verification would pay. Disabled records used to
			// short-circuit before any crypto work, letting response timing
			// distinguish them from unknown/active accounts.
			dummyHash(req.Password)
		}
		s.loginFailed(key)
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	tok, terr := newToken(32)
	if terr != nil {
		writeErr(w, 500, "session error")
		return
	}
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
		Secure: s.secureCookies(),
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
	tok, terr := newToken(32)
	if terr != nil {
		writeErr(w, 500, "session error")
		return
	}
	if err := s.st.CreateSession(u.ID, tok, "fabric-sso", s.cfg.SessionTTL); err != nil {
		writeErr(w, 500, "session error")
		return
	}
	s.st.AddEvent(u.ID, "login", "fabric SSO from "+clientIP(r))
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(s.cfg.SessionTTL.Seconds()),
		Secure: s.secureCookies(),
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

// handlePasswordChange is self-service password rotation for the logged-in
// user: POST /api/auth/password {"current_password","new_password"} → 200 {}.
// The current password is verified against the stored hash (401 on mismatch)
// so a hijacked session cannot silently lock out the real owner. New
// passwords follow the same minimum rule as admin-set passwords.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid request body")
		return
	}
	cur, err := s.st.UserByID(u.ID)
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	if cur.FabricID != "" {
		writeErr(w, 400, "estate accounts change their password at the identity provider")
		return
	}
	if !store.CheckPassword(cur.PasswordHash, req.CurrentPassword) {
		writeErr(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if len(req.NewPassword) < 8 {
		writeErr(w, 400, "password must be at least 8 characters")
		return
	}
	if err := s.st.SetPassword(u.ID, hashPassword(req.NewPassword)); err != nil {
		writeErr(w, 500, "error")
		return
	}
	s.st.AddEvent(u.ID, "change_password", "")
	writeJSON(w, http.StatusOK, map[string]any{})
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
		// Open (Lstat-guarded) BEFORE writing the header so symlinks,
		// specials and unreadable files are excluded from the archive
		// entirely instead of appearing as phantom empty entries.
		f, _, ferr := fsdrv.OpenRegular(abs)
		if ferr != nil {
			return nil // skip silently; one bad entry must not fail the zip
		}
		hdr := &zip.FileHeader{Name: relInZip, Method: zip.Deflate, Modified: time.Unix(e.ModTime, 0)}
		hdr.SetMode(0o644)
		fw, cerr := zw.CreateHeader(hdr)
		if cerr != nil {
			f.Close()
			return cerr
		}
		_, cerr = io.Copy(fw, f)
		f.Close()
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
	// history follows the file: index rows + blob dirs, subtree included
	// (renaming a directory moves every descendant's versions too)
	s.relocateVersionHistory(u.ID, u.Username, oldLogical, entry.Path)
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
	s.relocateVersionHistory(u.ID, u.Username, oldLogical, entry.Path)
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
	// Record exactly which version rows belong to this path BEFORE trashing.
	// The list travels in the trash metadata so a later restore onto a
	// REUSED path moves only this history — never a newer occupant's rows.
	logical := mustValidate(req.Path)
	var owned []fsdrv.TrashVersionRef
	if refs, err := s.st.VersionsUnder(u.ID, logical); err == nil {
		for _, ref := range refs {
			owned = append(owned, fsdrv.TrashVersionRef{Path: ref.Path, VersionID: ref.VersionID})
		}
	}
	id, orig, err := s.fs.DeleteWithVersions(u.Username, req.Path, owned)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	s.st.DeleteEtagsUnder(u.ID, orig)
	// Version rows/blobs are deliberately KEPT while the item sits in the
	// trash: restore must bring the history back. They are dropped only at
	// permanent-purge sites (handleTrashDelete / handleTrashEmpty).
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
	// Read the original path BEFORE Restore consumes the metadata sidecar.
	meta, merr := s.fs.ReadTrashMeta(u.Username, req.ID)
	// Snapshot EXACTLY which version rows belong to the trashed item before
	// anything changes. A NEWER file may occupy OrigPath by now and have
	// accumulated its own history there; only the rows recorded at delete
	// time may follow the restore — relocating everything under the path
	// would merge two files' histories and strip the live occupant's.
	var owned []store.VersionRef
	if merr == nil && meta != nil && meta.OrigPath != "" {
		if len(meta.Versions) > 0 {
			for _, ref := range meta.Versions {
				owned = append(owned, store.VersionRef{Path: ref.Path, VersionID: ref.VersionID})
			}
		} else {
			// Legacy trash item written before delete-time ownership was
			// recorded: degrade to the previous whole-path relocation.
			refs, err := s.st.VersionsUnder(u.ID, meta.OrigPath)
			if err != nil {
				log.Printf("versions: snapshot before restore of %s: %v", meta.OrigPath, err)
			}
			owned = refs
		}
	}
	dest, err := s.fs.Restore(u.Username, req.ID)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	// If restore had to pick a numbered sibling (original path occupied), only
	// the captured rows travel with it; a same-path restore needs nothing.
	if len(owned) > 0 && merr == nil && meta != nil && meta.OrigPath != "" && meta.OrigPath != dest {
		s.moveSpecificVersions(u.ID, u.Username, meta.OrigPath, dest, owned)
	}
	s.st.AddEvent(u.ID, "restore", dest)
	writeJSON(w, 200, map[string]string{"restoredTo": dest})
}

// moveSpecificVersions relocates exactly the captured version rows — index
// first, then their individual blob files — from oldRoot to newRoot after a
// committed trash-restore. Failure handling matches relocateVersionHistory:
// logged and swallowed; it can never fail the primary operation.
func (s *Server) moveSpecificVersions(userID int64, username, oldRoot, newRoot string, refs []store.VersionRef) {
	if err := s.st.MoveSpecificVersions(userID, refs, oldRoot, newRoot); err != nil {
		log.Printf("versions: move index %s -> %s: %v", oldRoot, newRoot, err)
		return
	}
	moves := make([]fsdrv.BlobMove, 0, len(refs))
	for _, ref := range refs {
		if !strings.HasPrefix(ref.Path, oldRoot) {
			continue
		}
		moves = append(moves, fsdrv.BlobMove{
			From:      ref.Path,
			To:        newRoot + strings.TrimPrefix(ref.Path, oldRoot),
			VersionID: ref.VersionID,
		})
	}
	if len(moves) == 0 {
		return
	}
	if err := s.fs.RelocateVersionBlobs(username, moves); err != nil {
		log.Printf("versions: move blobs %s -> %s: %v", oldRoot, newRoot, err)
	}
}

func (s *Server) handleTrashDelete(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct{ ID string }
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	// Permanent purge: capture OrigPath first, then drop payload, then the
	// version index + blobs that belonged to it.
	meta, err := s.fs.ReadTrashMeta(u.Username, req.ID)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	if err := s.fs.PurgeTrashItem(u.Username, req.ID); err != nil {
		mapFsErr(w, err)
		return
	}
	switch {
	case len(meta.Versions) > 0:
		// Ownership was recorded at delete time: drop only this item's
		// versions. A newer file recreated at OrigPath keeps its history.
		refs := make([]store.VersionRef, 0, len(meta.Versions))
		for _, ref := range meta.Versions {
			refs = append(refs, store.VersionRef{Path: ref.Path, VersionID: ref.VersionID})
		}
		s.purgeSpecificVersions(u.ID, u.Username, refs)
	case meta.OrigPath != "":
		// Legacy trash item without ownership record: degrade to whole-path.
		s.purgeVersionHistory(u.ID, u.Username, meta.OrigPath)
	}
	s.st.AddEvent(u.ID, "purge", req.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleTrashEmpty(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	// Metadata must be read before EmptyTrash wipes the trash dir; items with
	// corrupt/missing metadata keep their versions (same skip rule as the janitor).
	items, _ := s.fs.ListTrash(u.Username)
	if err := s.fs.EmptyTrash(u.Username); err != nil {
		mapFsErr(w, err)
		return
	}
	for _, it := range items {
		switch {
		case len(it.Versions) > 0:
			refs := make([]store.VersionRef, 0, len(it.Versions))
			for _, ref := range it.Versions {
				refs = append(refs, store.VersionRef{Path: ref.Path, VersionID: ref.VersionID})
			}
			s.purgeSpecificVersions(u.ID, u.Username, refs)
		case it.OrigPath != "":
			s.purgeVersionHistory(u.ID, u.Username, it.OrigPath)
		}
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

// handleAdminDeleteUser permanently removes a disabled user: their files,
// trash, version blobs, and metadata row. Disable first; this is irreversible.
func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
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
		writeErr(w, 400, "cannot delete yourself")
		return
	}
	if !target.Disabled {
		writeErr(w, 400, "disable the account first")
		return
	}
	if err := s.fs.RemoveUser(target.Username); err != nil {
		log.Printf("admin delete: remove files for %s: %v", target.Username, err)
		writeErr(w, 500, "error")
		return
	}
	if err := s.st.DeleteUser(target.ID); err != nil {
		writeErr(w, 500, "error")
		return
	}
	s.invalidateShareIndex()
	s.st.AddEvent(UserFrom(r).ID, "admin_delete_user", target.Username)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- small helpers ----

// relocateVersionHistory moves version history from oldPath to newPath after
// a committed rename/move/restore: SQLite rows first (RelocateVersions
// returns the distinct old paths that had rows), then the matching blob
// directories. This ordering is the contract for ALL version-relocation call
// sites: the filesystem change has already committed, and a relocation
// failure is logged and swallowed — worst case some version becomes
// unreachable exactly as before this fix; it can never fail or roll back the
// primary operation, and a path with no history relocates nothing.
func (s *Server) relocateVersionHistory(userID int64, username, oldPath, newPath string) {
	if oldPath == "" || oldPath == "/" || oldPath == newPath {
		return
	}
	olds, err := s.st.RelocateVersions(userID, oldPath, newPath)
	if err != nil {
		log.Printf("versions: relocate index %s -> %s: %v", oldPath, newPath, err)
		return
	}
	for _, old := range olds {
		np := newPath + strings.TrimPrefix(old, oldPath)
		if err := s.fs.RelocateVersionDir(username, old, np); err != nil {
			log.Printf("versions: relocate blobs %s -> %s: %v", old, np, err)
		}
	}
}

// purgeVersionHistory permanently drops version index rows and blob dirs for
// a path and everything below it. Legacy fallback for trash items written
// before delete-time ownership was recorded; trashing keeps history.
func (s *Server) purgeVersionHistory(userID int64, username, origPath string) {
	paths, err := s.st.DeleteVersionsUnder(userID, origPath)
	if err != nil {
		log.Printf("versions: purge index under %s: %v", origPath, err)
		return
	}
	for _, p := range paths {
		if err := s.fs.DeleteVersionDir(username, p); err != nil {
			log.Printf("versions: purge blobs %s: %v", p, err)
		}
	}
}

// purgeSpecificVersions permanently drops exactly the recorded version rows
// and their individual blob files. Used when the trash metadata knows which
// versions the item owned, so a newer occupant of the same logical path is
// never touched.
func (s *Server) purgeSpecificVersions(userID int64, username string, refs []store.VersionRef) {
	paths, err := s.st.DeleteSpecificVersions(userID, refs)
	if err != nil {
		log.Printf("versions: purge specific index: %v", err)
		return
	}
	for _, ref := range refs {
		if !seenPath(paths, ref.Path) {
			continue // row was already gone
		}
		if err := s.fs.PruneVersionBlob(username, ref.Path, ref.VersionID); err != nil {
			log.Printf("versions: purge blob %s@%s: %v", ref.Path, ref.VersionID, err)
		}
	}
}

func seenPath(paths []string, p string) bool {
	for _, x := range paths {
		if x == p {
			return true
		}
	}
	return false
}

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
