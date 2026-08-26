package api

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"xxdrive/internal/fsdrv"
	"xxdrive/internal/store"
)

const shareGrantTTL = 24 * time.Hour

// pubGrantMaxEnts bounds the in-memory share-grant map: inserts beyond the
// cap evict the soonest-expiring grants, and everything older than
// shareGrantTTL is swept opportunistically on insert.
const pubGrantMaxEnts = 1024

// ---- authenticated share management ----

func (s *Server) handleShareList(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	shares, err := s.st.ListShares(u.ID)
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	type row struct {
		TokenHash     string `json:"tokenHash"`
		Path          string `json:"path"`
		HasPassword   bool   `json:"hasPassword"`
		AllowDownload bool   `json:"allowDownload"`
		ExpiresAt     int64  `json:"expiresAt"`
		CreatedAt     int64  `json:"createdAt"`
		URL           string `json:"url"`
	}
	out := make([]row, 0, len(shares))
	for _, sh := range shares {
		out = append(out, row{
			TokenHash: sh.TokenHash[:16], Path: sh.Path, HasPassword: sh.HasPassword,
			AllowDownload: sh.AllowDownload, ExpiresAt: sh.ExpiresAt, CreatedAt: sh.CreatedAt,
			URL: s.shareURLPrefix() + "/s/" + sh.TokenHash[:16],
		})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	var req struct {
		Path          string `json:"path"`
		Password      string `json:"password"`
		ExpiresInDays int    `json:"expiresInDays"`
		AllowDownload *bool  `json:"allowDownload"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	logical, err := fsdrv.ValidateRel(req.Path)
	if err != nil || logical == "/" {
		writeErr(w, 400, "invalid path")
		return
	}
	if _, err := s.fs.Stat(u.Username, logical); err != nil {
		mapFsErr(w, err)
		return
	}
	allowDownload := true
	if req.AllowDownload != nil {
		allowDownload = *req.AllowDownload
	}
	var pwHash []byte
	if req.Password != "" {
		pwHash = hashPassword(req.Password)
	}
	var expiresAt int64
	if req.ExpiresInDays > 0 {
		expiresAt = time.Now().AddDate(0, 0, req.ExpiresInDays).Unix()
	}
	token, terr := newToken(24)
	if terr != nil {
		writeErr(w, 500, "share error")
		return
	}
	sh, err := s.st.CreateShare(u.ID, logical, token, pwHash, allowDownload, expiresAt)
	if err != nil {
		writeErr(w, 500, "share error")
		return
	}
	s.st.AddEvent(u.ID, "share_create", logical)
	s.invalidateShareIndex()
	writeJSON(w, 200, map[string]any{
		"token":         token,
		"path":          sh.Path,
		"hasPassword":   sh.HasPassword,
		"allowDownload": sh.AllowDownload,
		"expiresAt":     sh.ExpiresAt,
	})
}

func (s *Server) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r)
	hash16 := r.PathValue("tokenHash")
	shares, _ := s.st.ListShares(u.ID)
	for _, sh := range shares {
		if strings.HasPrefix(sh.TokenHash, hash16) && len(hash16) == 16 {
			if err := s.st.RevokeShare(u.ID, sh.TokenHash); err != nil {
				writeErr(w, 500, "error")
				return
			}
			s.st.AddEvent(u.ID, "share_revoke", sh.Path)
			s.invalidateShareIndex()
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
	}
	writeErr(w, 404, "no such share")
}

func (s *Server) shareURLPrefix() string {
	if s.cfg.BaseURL != "" {
		return strings.TrimSuffix(s.cfg.BaseURL, "/")
	}
	return ""
}

// ---- public (anonymous) access ----

// resolveShare maps a path value to a live share. Accepts either a full raw
// capability token (canonical share URLs) or a 16-char hash prefix (short
// URLs shown in the management UI). Prefix lookups go through an in-memory
// index so short URLs do not scan every user's shares; the index is rebuilt
// lazily after any create/revoke and expiry is re-checked per hit, so
// revoked/expired shares never resolve.
func (s *Server) resolveShare(v string) (*store.Share, bool) {
	if v == "" {
		return nil, false
	}
	if len(v) > 16 {
		if sh, err := s.st.ShareByToken(v); err == nil {
			return sh, true
		}
	}
	if len(v) == 16 {
		if sh, ok := s.shareByPrefix(v); ok {
			return sh, true
		}
	}
	return nil, false
}

func (s *Server) shareByPrefix(prefix string) (*store.Share, bool) {
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	if s.shareIdxStale {
		idx, err := s.buildShareIndex()
		if err == nil {
			s.shareIdx = idx
			s.shareIdxStale = false
		}
		// On rebuild failure keep whatever map we already have rather than
		// 404 every short URL for the rest of the process.
	}
	sh, ok := s.shareIdx[prefix]
	if !ok {
		return nil, false
	}
	if sh.ExpiresAt != 0 && time.Now().Unix() >= sh.ExpiresAt {
		return nil, false
	}
	return sh, true
}

func (s *Server) buildShareIndex() (map[string]*store.Share, error) {
	all, err := s.allLiveShares()
	if err != nil {
		return nil, err
	}
	idx := make(map[string]*store.Share, len(all))
	for _, sh := range all {
		idx[sh.TokenHash[:16]] = sh
	}
	return idx, nil
}

// invalidateShareIndex forces the next prefix lookup to rebuild from the store.
func (s *Server) invalidateShareIndex() {
	s.shareMu.Lock()
	s.shareIdxStale = true
	s.shareMu.Unlock()
}

// allLiveShares scans every user's shares; share volume is small on personal instances.
func (s *Server) allLiveShares() ([]*store.Share, error) {
	users, err := s.st.ListUsers()
	if err != nil {
		return nil, err
	}
	var out []*store.Share
	for _, u := range users {
		shares, err := s.st.ListShares(u.ID)
		if err != nil {
			continue
		}
		out = append(out, shares...)
	}
	return out, nil
}

func (s *Server) grantCookieName(tokenHash string) string {
	return "xxd_pub_" + tokenHash[:12]
}

func (s *Server) hasGrant(r *http.Request, sh *store.Share) bool {
	c, err := r.Cookie(s.grantCookieName(sh.TokenHash))
	if err != nil {
		return false
	}
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	grants, ok := s.pubGr[sh.TokenHash]
	if !ok {
		return false
	}
	exp, ok := grants[c.Value]
	if !ok || time.Now().Unix() > exp {
		delete(grants, c.Value)
		return false
	}
	return true
}

func (s *Server) issueGrant(w http.ResponseWriter, sh *store.Share) {
	b := make([]byte, 24)
	rand.Read(b)
	grant := hex.EncodeToString(b)
	now := time.Now()
	s.pubMu.Lock()
	if s.pubGr[sh.TokenHash] == nil {
		s.pubGr[sh.TokenHash] = map[string]int64{}
	}
	s.pubGr[sh.TokenHash][grant] = now.Add(shareGrantTTL).Unix()
	s.sweepGrantsLocked(now)
	s.capGrantsLocked()
	s.pubMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: s.grantCookieName(sh.TokenHash), Value: grant, Path: "/s/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(shareGrantTTL.Seconds()),
		Secure: s.secureCookies(),
	})
}

// sweepGrantsLocked drops expired grants across every share. Caller holds pubMu.
func (s *Server) sweepGrantsLocked(now time.Time) {
	cutoff := now.Unix()
	for tok, grants := range s.pubGr {
		for g, exp := range grants {
			if exp <= cutoff {
				delete(grants, g)
			}
		}
		if len(grants) == 0 {
			delete(s.pubGr, tok)
		}
	}
}

// capGrantsLocked bounds total live grants by evicting the soonest-expiring
// entries first (newest grants — real visitors — survive link scraping).
// Caller holds pubMu.
func (s *Server) capGrantsLocked() {
	total := 0
	for _, grants := range s.pubGr {
		total += len(grants)
	}
	for total > pubGrantMaxEnts {
		var (
			tok, victim string
			oldest      int64 = math.MaxInt64
		)
		for t, grants := range s.pubGr {
			for g, exp := range grants {
				if exp < oldest {
					tok, victim, oldest = t, g, exp
				}
			}
		}
		if victim == "" {
			return
		}
		delete(s.pubGr[tok], victim)
		if len(s.pubGr[tok]) == 0 {
			delete(s.pubGr, tok)
		}
		total--
	}
}

// SweepShareGrants drops expired public-share grants; safe to call from a
// background janitor.
func (s *Server) SweepShareGrants() {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	s.sweepGrantsLocked(time.Now())
}

var pubPageTmpl = template.Must(template.New("pub").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Shared — {{.Title}}</title>
<style>
body{font-family:system-ui,sans-serif;margin:0;background:#0f1420;color:#e8ecf4}
header{padding:1rem 1.5rem;border-bottom:1px solid #232b3d;display:flex;gap:.75rem;align-items:center}
h1{font-size:1.05rem;margin:0;font-weight:600}
main{max-width:860px;margin:2rem auto;padding:0 1rem}
a{color:#7fb3ff;text-decoration:none}
ul{list-style:none;padding:0}
li{display:flex;justify-content:space-between;gap:1rem;padding:.6rem .9rem;border-bottom:1px solid #1c2333}
.btn{display:inline-block;background:#2f6fed;color:#fff;border:none;border-radius:8px;padding:.55rem 1.1rem;font-size:.95rem;cursor:pointer;text-decoration:none}
form{display:flex;gap:.5rem;max-width:380px;margin:20vh auto}
input{flex:1;background:#161d2c;border:1px solid #2a3348;color:#e8ecf4;border-radius:8px;padding:.6rem .8rem;font-size:1rem}
.err{color:#ff8080;text-align:center}
img.prev{max-width:100%;border-radius:10px;margin-top:1rem}
video.prev{max-width:100%;border-radius:10px;margin-top:1rem}
</style></head><body>
<header><strong>xx-drive</strong><h1>{{.Title}}</h1>{{if and .AllowDownload (not .NeedPw)}}<span style="margin-left:auto"><a class="btn" href="{{.DownloadURL}}">Download{{if .IsDir}} all (.zip){{end}}</a></span>{{end}}</header>
<main>
{{if .NeedPw}}<form method="post" action="{{.FormAction}}">
{{if .Sub}}<input type="hidden" name="sub" value="{{.Sub}}">{{end}}
<input type="password" name="password" placeholder="Password" autocomplete="off" autofocus required>
<button class="btn" type="submit">Unlock</button>
</form>{{end}}
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if not .NeedPw}}{{if .Entries}}<ul>{{range .Entries}}
<li><a href="{{.URL}}">{{.Name}}</a><small>{{.SizeLabel}}</small></li>
{{end}}</ul>{{end}}
{{if .PreviewImage}}<img class="prev" src="{{.PreviewImage}}" alt="{{.Title}}">
{{else if .PreviewVideo}}<video class="prev" controls src="{{.PreviewVideo}}"></video>{{end}}{{end}}
</main></body></html>`))

type pubEntryView struct {
	Name, URL, SizeLabel string
}

func sizeLabel(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func isImage(name string) bool {
	switch strings.ToLower(filepathExt(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".bmp":
		return true
	}
	return false
}

func isVideo(name string) bool {
	switch strings.ToLower(filepathExt(name)) {
	case ".mp4", ".webm", ".mov", ".m4v":
		return true
	}
	return false
}

func filepathExt(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	return name[i:]
}

// guardShare authenticates an anonymous visitor to a share.
// Returns nil after writing an error/password response.
func (s *Server) guardShare(w http.ResponseWriter, r *http.Request) *store.Share {
	sh, ok := s.resolveShare(r.PathValue("token"))
	if !ok {
		writeErr(w, http.StatusNotFound, "share not found or expired")
		return nil
	}
	if sh.HasPassword && !s.hasGrant(r, sh) {
		// For API subrequests demand 401 JSON; for page GET render password form.
		if strings.HasSuffix(r.URL.Path, "/list") || strings.HasSuffix(r.URL.Path, "/download") {
			writeErr(w, http.StatusUnauthorized, "password required")
			return nil
		}
		s.renderPubPage(w, r, sh, "", true, "")
		return nil
	}
	return sh
}

func (s *Server) renderPubPage(w http.ResponseWriter, r *http.Request, sh *store.Share, sub string, needPw bool, errMsg string) {
	owner, err := s.st.UserByID(sh.UserID)
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	token := r.PathValue("token")
	full := sh.Path
	if sub != "" {
		full += sub
	}
	// Defensive re-check: callers resolve `sub` through shareTarget, but the
	// render path must never escape the share even if a future caller forgets.
	if full != sh.Path && !strings.HasPrefix(full, sh.Path+"/") {
		http.NotFound(w, r)
		return
	}
	data := struct {
		Title         string
		Error         string
		Entries       []pubEntryView
		IsDir         bool
		AllowDownload bool
		DownloadURL   string
		PreviewImage  string
		PreviewVideo  string
		NeedPw        bool
		FormAction    string
		Sub           string
	}{
		Title: fsdrv.NameOf(sh.Path), AllowDownload: sh.AllowDownload, Error: errMsg,
		NeedPw: needPw, FormAction: "/s/" + token, Sub: sub,
	}
	if data.Title == "" {
		data.Title = "/"
	}
	dl := fmt.Sprintf("/s/%s/download?sub=%s", token, urlQueryEscape(sub))
	data.DownloadURL = dl
	if !needPw {
		e, serr := s.fs.Stat(owner.Username, full)
		if serr == nil && !e.IsDir {
			data.IsDir = false
			if isImage(e.Name) {
				data.PreviewImage = dl + "&inline=1"
			} else if isVideo(e.Name) {
				data.PreviewVideo = dl + "&inline=1"
			}
		} else if serr == nil {
			data.IsDir = true
			ents, lerr := s.fs.List(owner.Username, full)
			if lerr == nil {
				for _, en := range ents {
					// Both files and folders link to the HTML page; the page
					// renders previews + Download for files and a listing for
					// directories. `sub` stays relative to the share root.
					u := fmt.Sprintf("/s/%s?sub=%s", token, urlQueryEscape(strings.TrimPrefix(en.Path, sh.Path)))
					lbl := sizeLabel(en.Size)
					if en.IsDir {
						lbl = "folder"
					}
					data.Entries = append(data.Entries, pubEntryView{Name: en.Name, URL: u, SizeLabel: lbl})
				}
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pubPageTmpl.Execute(w, data)
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// shareTarget resolves what a visitor asked for within a share. Accepts BOTH
// `path` (full logical path) and `sub` (relative to the share root); returns
// the resolved logical path plus the share-relative sub path. ok=false unless
// the target sits AT or UNDER the share root — the mandatory containment check
// that keeps ?path=/otheruser/x and ?sub=../../x out of the share.
func shareTarget(sh *store.Share, rawPath, rawSub string) (full, sub string, ok bool) {
	switch {
	case rawPath != "":
		if hasDotDotSegment(rawPath) {
			return "", "", false
		}
		v, err := fsdrv.ValidateRel(rawPath)
		if err != nil {
			return "", "", false
		}
		full, sub = v, strings.TrimPrefix(v, sh.Path)
	case rawSub != "":
		if hasDotDotSegment(rawSub) {
			return "", "", false
		}
		v, err := fsdrv.ValidateRel(rawSub)
		if err != nil {
			return "", "", false
		}
		switch {
		case v == "/":
			full = sh.Path
		case v == sh.Path || strings.HasPrefix(v, sh.Path+"/"):
			// Absolute sub that already includes the share root (old clients
			// emit the full logical path): use as-is instead of doubling the
			// prefix (/photos + /photos/vacation).
			full, sub = v, strings.TrimPrefix(v, sh.Path)
		default:
			full, sub = sh.Path+v, v
		}
	default:
		return sh.Path, "", true
	}
	if full != sh.Path && !strings.HasPrefix(full, sh.Path+"/") {
		return "", "", false
	}
	return full, sub, true
}

// hasDotDotSegment rejects literal ".." traversal before ValidateRel's Clean
// would silently fold it into an in-share path.
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func (s *Server) handlePublicPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sh := s.guardSharePage(w, r)
	if sh == nil {
		return
	}
	_, sub, ok := shareTarget(sh, q.Get("path"), q.Get("sub"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.renderPubPage(w, r, sh, sub, false, "")
}

// guardSharePage renders the password form when needed instead of JSON errors.
func (s *Server) guardSharePage(w http.ResponseWriter, r *http.Request) *store.Share {
	sh, ok := s.resolveShare(r.PathValue("token"))
	if !ok {
		http.NotFound(w, r)
		return nil
	}
	if sh.HasPassword && !s.hasGrant(r, sh) {
		_, sub, valid := shareTarget(sh, r.URL.Query().Get("path"), r.URL.Query().Get("sub"))
		if !valid {
			sub = ""
		}
		s.renderPubPage(w, r, sh, sub, true, "")
		return nil
	}
	return sh
}

func (s *Server) handlePublicPassword(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.resolveShare(r.PathValue("token"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Throttle password guesses per client IP + share. Every guess burns a
	// full PBKDF2 verification unauthenticated; without the bucket an online
	// brute force doubles as a CPU-exhaustion vector. Same limits as login
	// (10 failures / 15 min), 429 when exhausted.
	key := clientIP(r) + "|" + sh.TokenHash
	if !s.loginAllowed(key) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts; try later")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, 400, "bad form")
		return
	}
	if s.st.CheckSharePassword(sh, r.PostFormValue("password")) {
		s.issueGrant(w, sh)
		target := "/s/" + r.PathValue("token")
		if _, sub, ok := shareTarget(sh, "", r.PostFormValue("sub")); ok && sub != "" {
			target += "?sub=" + urlQueryEscape(sub)
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	s.loginFailed(key)
	// Wrong-password re-render must keep ?sub= context (the hidden form field
	// already carries it through the success path); containment is re-checked.
	errSub := ""
	if _, sub, ok := shareTarget(sh, "", r.PostFormValue("sub")); ok {
		errSub = sub
	}
	s.renderPubPage(w, r, sh, errSub, true, "Wrong password")
}

func (s *Server) handlePublicList(w http.ResponseWriter, r *http.Request) {
	sh := s.guardShare(w, r)
	if sh == nil {
		return
	}
	owner, err := s.st.UserByID(sh.UserID)
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	q := r.URL.Query()
	full, _, ok := shareTarget(sh, q.Get("path"), q.Get("sub"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not part of this share")
		return
	}
	e, err := s.fs.Stat(owner.Username, full)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	if !e.IsDir {
		writeJSON(w, 200, map[string]any{"entry": e, "entries": []any{}})
		return
	}
	ents, err := s.fs.List(owner.Username, full)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"entry": e, "entries": ents})
}

func (s *Server) handlePublicDownload(w http.ResponseWriter, r *http.Request) {
	sh := s.guardShare(w, r)
	if sh == nil {
		return
	}
	owner, err := s.st.UserByID(sh.UserID)
	if err != nil {
		writeErr(w, 500, "error")
		return
	}
	q := r.URL.Query()
	full, _, ok := shareTarget(sh, q.Get("path"), q.Get("sub"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not part of this share")
		return
	}
	inline := q.Get("inline") == "1"

	name := fsdrv.NameOf(full)
	if name == "" {
		writeErr(w, 400, "bad path")
		return
	}
	e, err := s.fs.Stat(owner.Username, full)
	if err != nil {
		mapFsErr(w, err)
		return
	}
	// view-only shares: inline previews only for a small image/video
	// allowlist; everything else — including whole-folder zips and
	// &inline=1 on documents/archives — is refused.
	if !sh.AllowDownload {
		previewable := inline && (isImage(name) || isVideo(name))
		if !previewable {
			writeErr(w, http.StatusForbidden, "downloads disabled by owner")
			return
		}
	}
	if e.IsDir {
		// zip the shared subtree
		s.zipShared(w, r, owner.Username, full, name)
		return
	}
	s.serveFileDownload(w, r, func() (*os.File, os.FileInfo, error) {
		f, fi, _, ferr := s.fs.OpenFile(owner.Username, full)
		return f, fi, ferr
	}, name, inline)
}

func (s *Server) zipShared(w http.ResponseWriter, r *http.Request, username, root, name string) {
	zw := zip.NewWriter(w)
	defer zw.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename*=UTF-8''%s.zip`, urlPathEscape(name)))
	_ = s.fs.WalkTree(username, root, func(e *fsdrv.Entry, abs string) error {
		rel := strings.TrimPrefix(strings.TrimPrefix(e.Path, root), "/")
		if rel == "" {
			return nil
		}
		if e.IsDir {
			_, err := zw.CreateHeader(&zip.FileHeader{Name: rel + "/", Modified: time.Unix(e.ModTime, 0)})
			return err
		}
		hdr := &zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: time.Unix(e.ModTime, 0)}
		hdr.SetMode(0o644)
		// Open (Lstat-guarded) BEFORE writing the header so planted symlinks,
		// specials and unreadable files are excluded from the archive entirely.
		f, _, ferr := fsdrv.OpenRegular(abs)
		if ferr != nil {
			return nil // skip silently; one bad entry must not fail the zip
		}
		defer f.Close()
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = io.Copy(fw, f)
		return err
	})
}
