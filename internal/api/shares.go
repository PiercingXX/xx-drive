package api

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"xxdrive/internal/fsdrv"
	"xxdrive/internal/store"
)

const shareGrantTTL = 24 * time.Hour

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
	token := newToken(24)
	sh, err := s.st.CreateShare(u.ID, logical, token, pwHash, allowDownload, expiresAt)
	if err != nil {
		writeErr(w, 500, "share error")
		return
	}
	s.st.AddEvent(u.ID, "share_create", logical)
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
// URLs shown in the management UI).
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
		all, err := s.allLiveShares()
		if err != nil {
			return nil, false
		}
		for _, sh := range all {
			if strings.HasPrefix(sh.TokenHash, v) {
				return sh, true
			}
		}
	}
	return nil, false
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
	s.pubMu.Lock()
	if s.pubGr[sh.TokenHash] == nil {
		s.pubGr[sh.TokenHash] = map[string]int64{}
	}
	s.pubGr[sh.TokenHash][grant] = time.Now().Add(shareGrantTTL).Unix()
	s.pubMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: s.grantCookieName(sh.TokenHash), Value: grant, Path: "/s/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(shareGrantTTL.Seconds()),
	})
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
<header><strong>xx-drive</strong><h1>{{.Title}}</h1>{{if .AllowDownload}}<span style="margin-left:auto"><a class="btn" href="{{.DownloadURL}}">Download{{if .IsDir}} all (.zip){{end}}</a></span>{{end}}</header>
<main>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Entries}}<ul>{{range .Entries}}
<li><a href="{{.URL}}">{{.Name}}</a><small>{{.SizeLabel}}</small></li>
{{end}}</ul>{{end}}
{{if .PreviewImage}}<img class="prev" src="{{.PreviewImage}}" alt="{{.Title}}">
{{else if .PreviewVideo}}<video class="prev" controls src="{{.PreviewVideo}}"></video>{{end}}
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
	data := struct {
		Title         string
		Error         string
		Entries       []pubEntryView
		IsDir         bool
		AllowDownload bool
		DownloadURL   string
		PreviewImage  string
		PreviewVideo  string
	}{
		Title: fsdrv.NameOf(sh.Path), AllowDownload: sh.AllowDownload, Error: errMsg,
	}
	if data.Title == "" {
		data.Title = "/"
	}
	dl := fmt.Sprintf("/s/%s/download?path=%s", r.PathValue("token"), urlQueryEscape(sub))
	data.DownloadURL = dl
	if !needPw {
		e, serr := s.fs.Stat(owner.Username, joinSub(sh.Path, sub))
		if serr == nil && !e.IsDir {
			data.IsDir = false
			if isImage(e.Name) {
				data.PreviewImage = dl + "&inline=1"
			} else if isVideo(e.Name) {
				data.PreviewVideo = dl + "&inline=1"
			}
		} else if serr == nil {
			data.IsDir = true
			ents, lerr := s.fs.List(owner.Username, joinSub(sh.Path, sub))
			if lerr == nil {
				for _, en := range ents {
					u := fmt.Sprintf("/s/%s/list?sub=%s", r.PathValue("token"), urlQueryEscape(en.Path))
					if !en.IsDir {
						u = fmt.Sprintf("/s/%s/download?path=%s&inline=1", r.PathValue("token"), urlQueryEscape(en.Path))
					}
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

func joinSub(root, sub string) string {
	sub = mustValidate(sub)
	if sub == "/" {
		return root
	}
	return root + sub
}

func (s *Server) handlePublicPage(w http.ResponseWriter, r *http.Request) {
	sh, blocked := s.guardSharePage(w, r)
	if sh == nil {
		return
	}
	sub := r.URL.Query().Get("sub")
	s.renderPubPage(w, r, sh, sub, false, "")
	_ = blocked
}

// guardSharePage renders the password form when needed instead of JSON errors.
func (s *Server) guardSharePage(w http.ResponseWriter, r *http.Request) (*store.Share, bool) {
	sh, ok := s.resolveShare(r.PathValue("token"))
	if !ok {
		http.NotFound(w, r)
		return nil, false
	}
	if sh.HasPassword && !s.hasGrant(r, sh) {
		s.renderPubPage(w, r, sh, "", true, "")
		return nil, false
	}
	return sh, true
}

func (s *Server) handlePublicPassword(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.resolveShare(r.PathValue("token"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, 400, "bad form")
		return
	}
	if s.st.CheckSharePassword(sh, r.PostFormValue("password")) {
		s.issueGrant(w, sh)
		http.Redirect(w, r, "/s/"+r.PathValue("token"), http.StatusSeeOther)
		return
	}
	s.renderPubPage(w, r, sh, "", true, "Wrong password")
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
	full := joinSub(sh.Path, r.URL.Query().Get("sub"))
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
	sub := q.Get("sub")
	full := joinSub(sh.Path, sub)
	inline := q.Get("inline") == "1"

	// view-only shares: only inline previews allowed
	if inline && !sh.AllowDownload {
		// allowed: preview streaming
	} else if !sh.AllowDownload {
		writeErr(w, http.StatusForbidden, "downloads disabled by owner")
		return
	}

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
	if e.IsDir {
		if !sh.AllowDownload {
			writeErr(w, http.StatusForbidden, "downloads disabled by owner")
			return
		}
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
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(abs)
		if err != nil {
			return nil // skip unreadable
		}
		defer f.Close()
		_, err = io.Copy(fw, f)
		return err
	})
}
