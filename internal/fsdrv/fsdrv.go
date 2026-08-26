// Package fsdrv implements the plain-files-on-disk storage driver.
//
// Layout under the data root:
//
//	files/<username>/...          each user's drive tree (plain files, rsync-friendly)
//	trash/<username>/<id>/        deleted item payload (file or whole directory)
//	trash/<username>/<id>.json    {origPath, name, isDir, deletedAt}
//	versions/<user>/<sha256(path)>/<versionID>   prior content of a file
//	tmp/                          staging for atomic uploads
//
// SECURITY: every path that originates from a request must pass through
// ResolveUserPath exactly once. It rejects traversal, absolute paths,
// NUL bytes and verifies the final lexical target stays inside the user root.
package fsdrv

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrInvalid  = errors.New("invalid path")
	ErrNotEmpty = errors.New("directory not empty")
	ErrTooLarge = errors.New("file too large")
	ErrConflict = errors.New("conflict")
)

const versionKeep = 32

// VersionBlobMu serializes version-blob REMOVAL against in-process OPEN of the
// same blobs (janitor/prune vs concurrent restore/download). Readers
// (OpenVersion) hold the read side only across path-resolve+open; every
// removal site — pruned-blob delete, version-dir delete, version-dir relocate
// — holds the write side. Once a blob is successfully open, its file
// descriptor stays readable even if the file is unlinked afterwards, so
// streaming needs no lock. This is a pragmatic single-process mitigation:
// cross-process locking on a shared data dir remains unsupported.
var VersionBlobMu sync.RWMutex

// VersionInfo describes a snapshotted prior version of a file.
type VersionInfo struct {
	VersionID string `json:"versionId"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"createdAt"`
}

type Driver struct {
	root string // absolute, symlink-resolved data root
}

func New(root string) (*Driver, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"files", "trash", "versions", "tmp"} {
		if err := os.MkdirAll(filepath.Join(real, sub), 0o700); err != nil {
			return nil, err
		}
	}
	return &Driver{root: real}, nil
}

func (d *Driver) Root() string { return d.root }

// RemoveUser deletes files/, trash/, and versions/ for a single user. The
// username must be a clean path segment (same rules as CreateUser) so this
// can never RemoveAll a parent of the data root.
func (d *Driver) RemoveUser(username string) error {
	if username == "" || username == "." || username == ".." ||
		strings.ContainsAny(username, "/\\\x00") || filepath.Clean(username) != username {
		return ErrInvalid
	}
	for _, sub := range []string{"files", "trash", "versions"} {
		p := filepath.Join(d.root, sub, username)
		want := d.root + string(os.PathSeparator) + sub + string(os.PathSeparator) + username
		if p != want {
			return ErrInvalid
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// ---- path safety choke point ----

// ValidateRel checks a user-supplied relative path without touching disk.
// Returns the cleaned form which always begins with "/" (drive-root relative).
func ValidateRel(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", ErrInvalid
	}
	if rel == "" {
		return "/", nil
	}
	// Work in slash form; reject Windows-style separators explicitly so
	// `a\..\..` can never smuggle a separator through on Unix.
	if strings.Contains(rel, "\\") {
		return "", ErrInvalid
	}
	clean := filepath.Clean("/" + rel) // anchors and cleans; ".." above root collapses to "/"
	if clean == "." || !strings.HasPrefix(clean, "/") {
		return "/", nil
	}
	// Reject percent-encoded separators/dots in any segment: net/http decodes
	// once, so these are literal characters — but they are a classic
	// double-decode confusion vector downstream. Cheap to forbid.
	lower := strings.ToLower(clean)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%2e") {
		return "", ErrInvalid
	}
	for _, seg := range strings.Split(clean, "/") {
		switch seg {
		case "", ".", "..":
			continue
		}
		if len(seg) > 255 {
			return "", ErrInvalid
		}
	}
	if len(clean) > 4096 {
		return "", ErrInvalid
	}
	return clean, nil
}

// ResolveUserPath maps a validated drive-relative path onto the real filesystem
// inside files/<username> and guarantees containment. The second return is the
// cleaned logical path ("/" rooted).
//
// Defense-in-depth: every existing component is checked with Lstat and any
// symlink component aborts resolution, so content planted via shell access
// cannot be smuggled out of the user root through links.
func (d *Driver) ResolveUserPath(username, rel string) (abs string, logical string, err error) {
	logical, err = ValidateRel(rel)
	if err != nil {
		return "", "", err
	}
	// Containment is asserted against the PRE-CLEAN per-user directory: a
	// hostile username ("..", "a/../b", "…/") would be silently rewritten by
	// filepath.Join below and could then satisfy a post-clean containment
	// check while actually pointing outside files/. Comparing against the
	// literal uncleaned prefix rejects such names outright (defense in depth;
	// store.CreateUser already refuses them).
	expectedUserRoot := d.root + string(os.PathSeparator) + "files" + string(os.PathSeparator) + username
	userRoot := filepath.Join(d.root, "files", username)
	abs = filepath.Join(userRoot, filepath.FromSlash(logical))
	// Containment check on the lexical path.
	if abs != expectedUserRoot && !strings.HasPrefix(abs, expectedUserRoot+string(os.PathSeparator)) {
		return "", "", ErrInvalid
	}
	// Walk components from the user root down, rejecting symlinks.
	if strings.ContainsRune(rel, 0) {
		return "", "", ErrInvalid
	}
	cur := userRoot
	for _, seg := range strings.Split(strings.Trim(logical, "/"), "/") {
		if seg == "" {
			continue
		}
		next := filepath.Join(cur, seg)
		fi, lerr := os.Lstat(next)
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				return abs, logical, nil // remaining path doesn't exist yet — nothing more to check
			}
			return "", "", lerr
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", "", ErrInvalid // symlink component: refuse
		}
		if !fi.IsDir() {
			// a file component with more path below it can't exist;
			// keep walking anyway so callers get their own NotFound
			return abs, logical, nil
		}
		cur = next
	}
	return abs, logical, nil
}

// NameOf returns the final segment of a logical path ("", -> "" for root).
func NameOf(logical string) string {
	if logical == "/" {
		return ""
	}
	return filepath.Base(logical)
}

// EtagOf is the canonical weak etag format used for concurrency control.
func EtagOf(mtimeSec, size int64) string {
	return fmt.Sprintf(`"%x-%x"`, mtimeSec, size)
}

func ParentOf(logical string) string {
	if logical == "/" {
		return "/"
	}
	p := filepath.Dir(logical)
	if p == "." {
		return "/"
	}
	return p
}

// JoinPath joins two logical paths safely.
func JoinPath(dir, name string) (string, error) {
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." || strings.ContainsRune(name, 0) || name == "" {
		return "", ErrInvalid
	}
	if dir == "/" {
		return "/" + name, nil
	}
	return dir + "/" + name, nil
}

// ValidName enforces server-side filename rules for new names.
func ValidName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return false
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	if strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return false
	}
	return true
}

// ---- entry model ----

type Entry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
	Starred bool   `json:"starred,omitempty"`
}

// isStagingEntry reports whether name belongs to a client sync-staging file
// that must never surface as user content: the CLI stages downloads as
// "<name>.xxpart" and uploads historically use the ".xxpartial" prefix.
func isStagingEntry(name string) bool {
	return strings.HasPrefix(name, ".xxpartial") || strings.HasSuffix(name, ".xxpart")
}

// ---- operations ----

func (d *Driver) Stat(username, rel string) (*Entry, error) {
	abs, logical, err := d.ResolveUserPath(username, rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, mapStatErr(err)
	}
	return &Entry{Name: NameOf(logical), Path: logical, IsDir: fi.IsDir(), Size: fi.Size(), ModTime: fi.ModTime().Unix()}, nil
}

func (d *Driver) List(username, rel string) ([]*Entry, error) {
	abs, logical, err := d.ResolveUserPath(username, rel)
	if err != nil {
		return nil, err
	}
	fis, err := os.ReadDir(abs)
	if err != nil {
		return nil, mapStatErr(err)
	}
	out := make([]*Entry, 0, len(fis))
	for _, fi := range fis {
		name := fi.Name()
		if isStagingEntry(name) {
			continue
		}
		info, err := fi.Info()
		if err != nil {
			continue // raced delete
		}
		child := "/"
		if logical != "/" {
			child = logical + "/"
		}
		out = append(out, &Entry{
			Name: name, Path: child + name, IsDir: fi.IsDir(),
			Size: info.Size(), ModTime: info.ModTime().Unix(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (d *Driver) Mkdir(username, rel string) error {
	abs, logical, err := d.ResolveUserPath(username, rel)
	if err != nil {
		return err
	}
	if NameOf(logical) == "" {
		return ErrExists // root always exists
	}
	if err := os.MkdirAll(filepath.Join(d.root, "files", username), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		return mapStatErr(err)
	}
	return nil
}

// MkdirAllParents creates every missing parent of logical path,
// including the per-user root itself.
func (d *Driver) MkdirAllParents(username, logical string) error {
	parent := ParentOf(logical)
	abs, _, err := d.ResolveUserPath(username, parent)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, 0o755)
}

// Upload streams r into place atomically. If the destination exists:
//   - ifMatch == ""            → old content is pushed into version history, then overwritten
//   - ifMatch set and matches  → same as above (proves caller saw current state)
//   - ifMatch set and mismatch → returns ErrConflict, nothing written
//   - conflictRename           → writes a conflict-copy instead of overwriting
//
// Returns the sha256 of stored content and resulting entry.
func (d *Driver) Upload(username, rel string, r io.Reader, maxSize int64, ifMatch string, conflictRename bool, deviceTag string) (shaHex string, e *Entry, conflicted bool, prev VersionInfo, err error) {
	abs, logical, err := d.ResolveUserPath(username, rel)
	if err != nil {
		return "", nil, false, prev, err
	}
	if NameOf(logical) == "" {
		return "", nil, false, prev, ErrInvalid
	}

	// Stage to temp file first (atomic commit via rename).
	if err := os.MkdirAll(d.tmpDir(), 0o700); err != nil {
		return "", nil, false, prev, err
	}
	tmp, err := os.CreateTemp(d.tmpDir(), "up-*")
	if err != nil {
		return "", nil, false, prev, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, maxSize+1))
	if err != nil {
		return "", nil, false, prev, err
	}
	if size > maxSize {
		return "", nil, false, prev, ErrTooLarge
	}
	if err := tmp.Sync(); err != nil {
		return "", nil, false, prev, err
	}
	if err := tmp.Close(); err != nil {
		return "", nil, false, prev, err
	}
	shaHex = hex.EncodeToString(h.Sum(nil))

	// Existing target?
	existing, _ := os.Stat(abs)
	if existing != nil && existing.IsDir() {
		return "", nil, false, prev, ErrExists
	}
	target := abs
	if existing != nil {
		curEtag := EtagOf(existing.ModTime().Unix(), existing.Size())
		if conflictRename {
			base := NameOf(logical)
			ext := filepath.Ext(base)
			stem := strings.TrimSuffix(base, ext)
			stamp := time.Now().Format("2006-01-02 15:04:05")
			tag := deviceTag
			if tag == "" {
				tag = "another device"
			}
			np, jerr := JoinPath(ParentOf(logical), fmt.Sprintf("%s (conflict from %s %s)%s", stem, sanitizeTag(tag), stamp, ext))
			if jerr != nil {
				return "", nil, false, prev, jerr
			}
			// os.Rename atomically REPLACES its destination, so a synthesized
			// name that collides with an earlier conflict copy would silently
			// destroy it. Two uploads with the same basename+deviceTag inside
			// one second synthesize identical names; probe with Lstat and
			// disambiguate with an incrementing counter until free.
			for i := 2; ; i++ {
				probe, _, perr := d.ResolveUserPath(username, np)
				if perr != nil {
					return "", nil, false, prev, perr
				}
				_, lerr := os.Lstat(probe)
				if errors.Is(lerr, os.ErrNotExist) {
					break // free
				}
				if lerr != nil {
					return "", nil, false, prev, lerr
				}
				cext := filepath.Ext(np)
				np = strings.TrimSuffix(np, cext) + fmt.Sprintf(" (%d)", i) + cext
			}
			nabs, nlog, jerr := d.ResolveUserPath(username, np)
			if jerr != nil {
				return "", nil, false, prev, jerr
			}
			target, logical = nabs, nlog
			conflicted = true
		} else if ifMatch != "" && ifMatch != curEtag {
			return "", nil, false, prev, ErrConflict
		} else {
			// preserve current content as a version before overwrite
			vid, vsize, verr := d.snapshotVersion(username, logical, abs)
			if verr != nil {
				return "", nil, false, prev, verr
			}
			prev = VersionInfo{VersionID: vid, Size: vsize, CreatedAt: time.Now().Unix()}
		}
	}

	if err := d.MkdirAllParents(username, logical); err != nil {
		return "", nil, false, prev, err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return "", nil, false, prev, err
	}
	fi, err := os.Stat(target)
	if err != nil {
		return "", nil, false, prev, err
	}
	_ = os.Chmod(target, 0o644)
	return shaHex, &Entry{Name: NameOf(logical), Path: logical, IsDir: false, Size: fi.Size(), ModTime: fi.ModTime().Unix()}, conflicted, prev, nil
}

func sanitizeTag(t string) string {
	t = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			return '-'
		}
		return r
	}, t)
	if len(t) > 60 {
		t = t[:60]
	}
	return t
}

// OpenFile opens a file for reading (download). Caller closes.
func (d *Driver) OpenFile(username, rel string) (*os.File, os.FileInfo, string, error) {
	abs, logical, err := d.ResolveUserPath(username, rel)
	if err != nil {
		return nil, nil, "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, "", mapStatErr(err)
	}
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		f.Close()
		return nil, nil, "", ErrNotFound
	}
	return f, fi, logical, nil
}

func (d *Driver) Rename(username, from, toName string) (*Entry, error) {
	if !ValidName(toName) {
		return nil, ErrInvalid
	}
	srcAbs, srcLog, err := d.ResolveUserPath(username, from)
	if err != nil {
		return nil, err
	}
	if NameOf(srcLog) == "" {
		return nil, ErrInvalid // cannot rename root
	}
	dstLog, err := JoinPath(ParentOf(srcLog), toName)
	if err != nil {
		return nil, err
	}
	dstAbs, dstLog2, err := d.ResolveUserPath(username, dstLog)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(dstAbs); err == nil {
		return nil, ErrExists
	}
	if err := os.Rename(srcAbs, dstAbs); err != nil {
		return nil, mapStatErr(err)
	}
	fi, _ := os.Stat(dstAbs)
	return &Entry{Name: NameOf(dstLog2), Path: dstLog2, IsDir: fi.IsDir(), Size: fi.Size(), ModTime: fi.ModTime().Unix()}, nil
}

// Move relocates a subtree into destDir (keeping its name).
func (d *Driver) Move(username, from, destDir string) (*Entry, error) {
	srcAbs, srcLog, err := d.ResolveUserPath(username, from)
	if err != nil {
		return nil, err
	}
	_, dstDirLog, err := d.ResolveUserPath(username, destDir)
	if err != nil {
		return nil, err
	}
	if NameOf(srcLog) == "" {
		return nil, ErrInvalid
	}
	// forbid moving into itself
	if srcLog == dstDirLog || strings.HasPrefix(dstDirLog, srcLog+"/") {
		return nil, ErrInvalid
	}
	name := NameOf(srcLog)
	dstLog, err := JoinPath(dstDirLog, name)
	if err != nil {
		return nil, err
	}
	dstAbs, dstLog2, err := d.ResolveUserPath(username, dstLog)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(dstAbs); err == nil {
		return nil, ErrExists
	}
	if err := os.Rename(srcAbs, dstAbs); err != nil {
		return nil, mapStatErr(err)
	}
	fi, _ := os.Stat(dstAbs)
	return &Entry{Name: name, Path: dstLog2, IsDir: fi.IsDir(), Size: fi.Size(), ModTime: fi.ModTime().Unix()}, nil
}

// Copy duplicates a file or deep-copies a directory tree.
func (d *Driver) Copy(username, from, destDir string, renameTo string) (*Entry, error) {
	srcAbs, srcLog, err := d.ResolveUserPath(username, from)
	if err != nil {
		return nil, err
	}
	if NameOf(srcLog) == "" {
		return nil, ErrInvalid
	}
	_, dstDirLog, err := d.ResolveUserPath(username, destDir)
	if err != nil {
		return nil, err
	}
	name := NameOf(srcLog)
	if renameTo != "" {
		if !ValidName(renameTo) {
			return nil, ErrInvalid
		}
		name = renameTo
	}
	dstLog, err := JoinPath(dstDirLog, name)
	if err != nil {
		return nil, err
	}
	dstAbs, dstLog2, err := d.ResolveUserPath(username, dstLog)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(dstAbs); err == nil {
		return nil, ErrExists
	}
	fi, err := os.Stat(srcAbs)
	if err != nil {
		return nil, mapStatErr(err)
	}
	if fi.IsDir() {
		if err := copyTree(srcAbs, dstAbs); err != nil {
			return nil, err
		}
	} else {
		if err := copyFile(srcAbs, dstAbs, fi.Mode()); err != nil {
			return nil, err
		}
	}
	nfi, _ := os.Stat(dstAbs)
	return &Entry{Name: name, Path: dstLog2, IsDir: fi.IsDir(), Size: nfi.Size(), ModTime: nfi.ModTime().Unix()}, nil
}

// Delete moves a subtree into the trash. Returns a trash ID.
//
// Atomicity protocol (chosen design: metadata-first sidecar):
//
//  1. write metadata to trash/<user>/<id>.json.tmp   (sidecar, not yet live)
//  2. rename the payload into place                  trash/<user>/<id>
//  3. atomically publish the metadata                .json.tmp -> <id>.json
//
// ListTrash and the janitor only ever consult published <id>.json files, so
// an interrupted Delete leaves at most an unreferenced payload plus a .tmp
// sidecar — never a vanished original. Every failure path rolls back (payload
// renamed home, temporaries removed) so the original file is never lost while
// the operation reports failure. Only a double fault — the rollback rename
// itself failing, e.g. because something re-created the source path — can
// strand the payload inside the trash without metadata.
func (d *Driver) Delete(username, rel string) (id string, origPath string, err error) {
	return d.DeleteWithVersions(username, rel, nil)
}

// DeleteWithVersions moves a subtree into the trash, recording versionIds
// (the version-index rows owned by the item at delete time) in the metadata
// sidecar. A later restore onto a reused path can then move exactly this
// history and leave any newer occupant's rows alone.
func (d *Driver) DeleteWithVersions(username, rel string, versions []TrashVersionRef) (id string, origPath string, err error) {
	abs, logical, err := d.ResolveUserPath(username, rel)
	if err != nil {
		return "", "", err
	}
	if logical == "/" {
		return "", "", ErrInvalid
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return "", "", mapStatErr(err)
	}
	id, err = newID()
	if err != nil {
		return "", "", err
	}
	trashUserDir := filepath.Join(d.root, "trash", username)
	if err := os.MkdirAll(trashUserDir, 0o700); err != nil {
		return "", "", err
	}
	meta, _ := json.Marshal(map[string]any{
		"origPath":  logical,
		"name":      NameOf(logical),
		"isDir":     fi.IsDir(),
		"deletedAt": time.Now().Unix(),
		"versions":  versions,
	})
	// Step 1: metadata first — if this fails the payload never moved.
	sidecar := filepath.Join(trashUserDir, id+".json.tmp")
	if err := os.WriteFile(sidecar, meta, 0o600); err != nil {
		os.Remove(sidecar)
		return "", "", err
	}
	// Step 2: move the payload.
	trashItem := filepath.Join(trashUserDir, id)
	if err := os.Rename(abs, trashItem); err != nil {
		os.Remove(sidecar)
		return "", "", mapStatErr(err)
	}
	// Step 3: publish atomically; roll back on failure.
	if err := os.Rename(sidecar, trashItem+".json"); err != nil {
		os.Remove(sidecar)
		if rbErr := os.Rename(trashItem, abs); rbErr != nil {
			return "", "", fmt.Errorf("trash: finalize failed (%v) and rollback failed (%v); payload left at %s", err, rbErr, trashItem)
		}
		return "", "", err
	}
	return id, logical, nil
}

type TrashItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	OrigPath  string `json:"origPath"`
	IsDir     bool   `json:"isDir"`
	Size      int64  `json:"size"`
	DeletedAt int64  `json:"deletedAt"`
	// Versions records exactly which version-index rows belonged to the item
	// when it was trashed (path + version id). A NEW file later recreated at
	// OrigPath accumulates its own rows at the same key; without this record
	// a restore cannot tell the two histories apart. Empty for trash items
	// written before this field existed.
	Versions []TrashVersionRef `json:"versions,omitempty"`
}

// TrashVersionRef pins one version-index row to its logical path at
// delete time (subtree rows included for trashed directories).
type TrashVersionRef struct {
	Path      string `json:"path"`
	VersionID string `json:"versionId"`
}

func (d *Driver) ListTrash(username string) ([]*TrashItem, error) {
	dir := filepath.Join(d.root, "trash", username)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*TrashItem{}, nil
		}
		return nil, err
	}
	var out []*TrashItem
	for _, ent := range ents {
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		var m TrashItem
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		m.ID = strings.TrimSuffix(ent.Name(), ".json")
		if fi, err := os.Stat(filepath.Join(dir, m.ID)); err == nil && !m.IsDir {
			m.Size = fi.Size()
		}
		out = append(out, &m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeletedAt > out[j].DeletedAt })
	return out, nil
}

// Restore puts a trashed item back; collisions get numbered suffixes.
func (d *Driver) Restore(username, trashID string) (string, error) {
	if !validID(trashID) {
		return "", ErrInvalid
	}
	dir := filepath.Join(d.root, "trash", username)
	metaRaw, err := os.ReadFile(filepath.Join(dir, trashID+".json"))
	if err != nil {
		return "", ErrNotFound
	}
	var m struct {
		OrigPath string `json:"origPath"`
	}
	if json.Unmarshal(metaRaw, &m) != nil {
		return "", ErrNotFound
	}
	dest := m.OrigPath
	for i := 2; ; i++ {
		abs, log, err := d.ResolveUserPath(username, dest)
		if err != nil {
			return "", err
		}
		if _, err := os.Lstat(abs); errors.Is(err, os.ErrNotExist) {
			if err := d.MkdirAllParents(username, log); err != nil {
				return "", err
			}
			if err := os.Rename(filepath.Join(dir, trashID), abs); err != nil {
				return "", err
			}
			os.Remove(filepath.Join(dir, trashID+".json"))
			return log, nil
		} else if err != nil {
			return "", err
		}
		ext := filepath.Ext(dest)
		dest = strings.TrimSuffix(m.OrigPath, ext) + fmt.Sprintf(" (restored %d)%s", i, ext)
	}
}

// ReadTrashMeta loads one trash item's metadata without touching the payload.
// The API layer reads it BEFORE Restore/PurgeTrashItem consume the .json
// sidecar, so version history can follow (or be dropped for) OrigPath.
func (d *Driver) ReadTrashMeta(username, trashID string) (*TrashItem, error) {
	if !validID(trashID) {
		return nil, ErrInvalid
	}
	raw, err := os.ReadFile(filepath.Join(d.root, "trash", username, trashID+".json"))
	if err != nil {
		return nil, mapStatErr(err)
	}
	var m struct {
		OrigPath  string            `json:"origPath"`
		Name      string            `json:"name"`
		IsDir     bool              `json:"isDir"`
		DeletedAt int64             `json:"deletedAt"`
		Versions  []TrashVersionRef `json:"versions"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("trash: corrupt metadata for %s: %w", trashID, err)
	}
	return &TrashItem{ID: trashID, Name: m.Name, OrigPath: m.OrigPath, IsDir: m.IsDir,
		DeletedAt: m.DeletedAt, Versions: m.Versions}, nil
}

func (d *Driver) PurgeTrashItem(username, trashID string) error {
	if !validID(trashID) {
		return ErrInvalid
	}
	dir := filepath.Join(d.root, "trash", username)
	if _, err := os.Stat(filepath.Join(dir, trashID+".json")); err != nil {
		return ErrNotFound
	}
	if err := os.RemoveAll(filepath.Join(dir, trashID)); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, trashID+".json"))
}

func (d *Driver) EmptyTrash(username string) error {
	return os.RemoveAll(filepath.Join(d.root, "trash", username))
}

// minPlausibleUnix is the oldest DeletedAt we trust (2001-09-09). Anything at
// or below it (0 = Unix epoch, negatives, garbage) marks unusable metadata.
const minPlausibleUnix = 1_000_000_000

// PurgedTrash identifies a trash item the janitor removed permanently.
type PurgedTrash struct {
	Username string // trash owner (directory name under trash/)
	OrigPath string // drive-relative original path ("" if metadata lacked it)
}

// PurgeOldTrash removes trashed items older than cutoff and returns the
// purged items' identity, so callers can drop their version history (index
// rows + blob dirs) exactly like the interactive permanent-purge sites do.
//
// Safety: items whose metadata is unreadable or corrupt, and items whose
// deletedAt is implausible, are SKIPPED — never purged. Corrupt metadata must
// not destroy restorable payloads; only a plausible timestamp strictly older
// than the cutoff authorizes a purge.
func (d *Driver) PurgeOldTrash(cutoff time.Time) []*PurgedTrash {
	base := filepath.Join(d.root, "trash")
	users, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var purged []*PurgedTrash
	for _, u := range users {
		if !u.IsDir() {
			continue
		}
		dir := filepath.Join(base, u.Name())
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if !strings.HasSuffix(ent.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
			if err != nil {
				log.Printf("fsdrv: trash purge: cannot read %s/%s: %v; skipping", u.Name(), ent.Name(), err)
				continue
			}
			var m struct {
				DeletedAt int64  `json:"deletedAt"`
				OrigPath  string `json:"origPath"`
			}
			if json.Unmarshal(raw, &m) != nil {
				log.Printf("fsdrv: trash purge: corrupt metadata in %s/%s; skipping, payload preserved", u.Name(), ent.Name())
				continue
			}
			if m.DeletedAt < minPlausibleUnix {
				log.Printf("fsdrv: trash purge: implausible deletedAt=%d in %s/%s; skipping", m.DeletedAt, u.Name(), ent.Name())
				continue
			}
			if !time.Unix(m.DeletedAt, 0).Before(cutoff) {
				continue // not old enough yet
			}
			id := strings.TrimSuffix(ent.Name(), ".json")
			if os.RemoveAll(filepath.Join(dir, id)) == nil {
				os.Remove(filepath.Join(dir, ent.Name()))
				purged = append(purged, &PurgedTrash{Username: u.Name(), OrigPath: m.OrigPath})
			}
		}
	}
	return purged
}

// ---- versions ----

// RelocateVersionDir renames the on-disk version-blob directory for a logical
// path after that path was renamed/moved:
//
//	versions/<user>/<pathHash(old)> → versions/<user>/<pathHash(new)>
//
// A missing source directory means the path had no stored history — a no-op,
// never an error. If the destination already exists (both names carried
// history, e.g. stale rows) the blobs are merged file-by-file: version ids
// are random 32-hex identifiers so name collisions cannot occur.
func (d *Driver) RelocateVersionDir(username, oldLogical, newLogical string) error {
	// Write side of the blob race guard: an in-flight open of either name
	// completes before the directory moves (see VersionBlobMu).
	VersionBlobMu.Lock()
	defer VersionBlobMu.Unlock()
	vroot := filepath.Join(d.root, "versions", username)
	src := filepath.Join(vroot, pathHash(oldLogical))
	dst := filepath.Join(vroot, pathHash(newLogical))
	if _, err := os.Lstat(src); err != nil {
		return nil // no blobs stored for this path
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
		return os.Rename(src, dst)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, ent := range ents {
		if err := os.Rename(filepath.Join(src, ent.Name()), filepath.Join(dst, ent.Name())); err != nil &&
			!errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return os.Remove(src)
}

// DeleteVersionDir permanently removes every stored blob for a logical path
// (called only when a trash item is purged for good). A missing directory is
// not an error. Write side of the blob race guard: concurrent opens of these
// blobs complete first (see VersionBlobMu).
func (d *Driver) DeleteVersionDir(username, logical string) error {
	VersionBlobMu.Lock()
	defer VersionBlobMu.Unlock()
	return os.RemoveAll(filepath.Join(d.root, "versions", username, pathHash(logical)))
}

// BlobMove names one version blob to relocate between two logical paths.
type BlobMove struct {
	From      string // source logical path (blob dir key)
	To        string // destination logical path (blob dir key)
	VersionID string
}

// RelocateVersionBlobs moves individually listed version blobs between their
// logical paths' blob dirs (versions/<user>/<pathHash(path)>/<versionID>).
// Used when only PART of a path's stored history belongs to the item being
// moved — trash-restore onto a reused path must not drag a newer occupant's
// blobs along. Missing sources are not an error. Write side of the blob race
// guard (see VersionBlobMu).
func (d *Driver) RelocateVersionBlobs(username string, moves []BlobMove) error {
	VersionBlobMu.Lock()
	defer VersionBlobMu.Unlock()
	var firstErr error
	for _, mv := range moves {
		if !validID(mv.VersionID) || mv.From == "" || mv.To == "" {
			continue
		}
		src := filepath.Join(d.root, "versions", username, pathHash(mv.From), mv.VersionID)
		dstDir := filepath.Join(d.root, "versions", username, pathHash(mv.To))
		if err := os.MkdirAll(dstDir, 0o700); err != nil && firstErr == nil {
			firstErr = err
			continue
		}
		if err := os.Rename(src, filepath.Join(dstDir, mv.VersionID)); err != nil &&
			!errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// PruneVersionBlob removes one pruned version blob under VersionBlobMu — the
// janitor-side counterpart of OpenVersion's read lock. A missing file is not
// an error. When the blob's pathHash directory becomes empty it is removed
// too, so targeted purges leave no litter behind.
func (d *Driver) PruneVersionBlob(username, rel, versionID string) error {
	VersionBlobMu.Lock()
	defer VersionBlobMu.Unlock()
	blob := d.VersionBlobPath(username, rel, versionID)
	err := os.Remove(blob)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	// Best-effort: drop the per-path directory once its last blob is gone.
	// ENOTEMPTY (other versions of the same path remain) is expected and fine.
	_ = os.Remove(filepath.Dir(blob))
	return err
}

func (d *Driver) snapshotVersion(username, logical, abs string) (string, int64, error) {
	src, err := os.Open(abs)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return "", 0, err
	}
	vdir := filepath.Join(d.root, "versions", username, pathHash(logical))
	if err := os.MkdirAll(vdir, 0o700); err != nil {
		return "", 0, err
	}
	vid, err := newID()
	if err != nil {
		return "", 0, err
	}
	dst, err := os.OpenFile(filepath.Join(vdir, vid+".tmp"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	_, cpErr := io.Copy(dst, src)
	clErr := dst.Close()
	if cpErr != nil || clErr != nil {
		os.Remove(filepath.Join(vdir, vid+".tmp"))
		if cpErr != nil {
			return "", 0, cpErr
		}
		return "", 0, clErr
	}
	// atomic publish
	if err := os.Rename(filepath.Join(vdir, vid+".tmp"), filepath.Join(vdir, vid)); err != nil {
		return "", 0, err
	}
	return vid, fi.Size(), nil
}

// SnapshotCurrent records current content as an explicit version (used by API layer
// together with store.AddVersion for indexing).
func (d *Driver) SnapshotCurrent(username, rel string) (versionID string, size int64, err error) {
	abs, logical, err := d.ResolveUserPath(username, rel)
	if err != nil {
		return "", 0, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", 0, mapStatErr(err)
	}
	if fi.IsDir() {
		return "", 0, ErrInvalid
	}
	return d.snapshotVersion(username, logical, abs)
}

func (d *Driver) OpenVersion(username, rel, versionID string) (*os.File, int64, error) {
	if !validID(versionID) {
		return nil, 0, ErrInvalid
	}
	// Read side of the blob race guard (see VersionBlobMu): a concurrent
	// prune/purge must not unlink this blob between resolve and open.
	VersionBlobMu.RLock()
	defer VersionBlobMu.RUnlock()
	_, logical, err := d.ResolveUserPath(username, rel)
	if err != nil {
		return nil, 0, err
	}
	p := filepath.Join(d.root, "versions", username, pathHash(logical), versionID)
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, ErrNotFound
	}
	fi, _ := f.Stat()
	return f, fi.Size(), nil
}

// RestoreVersion replaces current content with a stored version.
// Current content is first snapshotted as a new version (restores are reversible).
func (d *Driver) RestoreVersion(username, rel, versionID string) error {
	src, size, err := d.OpenVersion(username, rel, versionID)
	if err != nil {
		return err
	}
	defer src.Close()
	_, _, _, _, err = d.Upload(username, rel, src, size+(1<<20), "", false, "")
	return err
}

// PruneOrphanVersions removes version blobs not present in the index (best effort).
func (d *Driver) VersionBlobPath(username, rel, versionID string) string {
	return filepath.Join(d.root, "versions", username, pathHash(rel), versionID)
}

// HashFile streams a file and returns its sha256 hex digest.
func (d *Driver) HashFile(username, rel string) (string, error) {
	f, _, _, err := d.OpenFile(username, rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- search ----

func (d *Driver) Search(username, query string, limit int) ([]*Entry, error) {
	q := strings.ToLower(query)
	// Route through the same containment check as every other entry point so
	// a hostile username can't point the walk outside files/.
	root, _, err := d.ResolveUserPath(username, "/")
	if err != nil {
		return nil, err
	}
	var out []*Entry
	stop := errors.New("stop")
	err = filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if len(out) >= limit {
			return stop
		}
		name := de.Name()
		if de.IsDir() {
			if strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.Contains(strings.ToLower(name), q) {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			info, ierr := de.Info()
			if ierr != nil {
				return nil
			}
			lp := "/" + filepath.ToSlash(rel)
			out = append(out, &Entry{Name: name, Path: lp, Size: info.Size(), ModTime: info.ModTime().Unix()})
		}
		return nil
	})
	if err != nil && err != stop {
		return nil, err
	}
	return out, nil
}

// WalkTree feeds every entry under logical prefix to fn (used by zip + sync).
func (d *Driver) WalkTree(username, rel string, fn func(e *Entry, abs string) error) error {
	absRoot, logicalRoot, err := d.ResolveUserPath(username, rel)
	if err != nil {
		return err
	}
	fi, err := os.Stat(absRoot)
	if err != nil {
		return mapStatErr(err)
	}
	if !fi.IsDir() {
		e := &Entry{Name: NameOf(logicalRoot), Path: logicalRoot, Size: fi.Size(), ModTime: fi.ModTime().Unix()}
		return fn(e, absRoot)
	}
	return filepath.WalkDir(absRoot, func(p string, de os.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		// Skip sync-staging files (parity with List); skip their whole
		// subtree if one names a directory.
		if isStagingEntry(de.Name()) {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relp, rerr := filepath.Rel(absRoot, p)
		if rerr != nil {
			return nil
		}
		lp := logicalRoot
		if relp != "." {
			lp = logicalRoot + "/" + filepath.ToSlash(relp)
		}
		info, ierr := de.Info()
		if ierr != nil {
			return nil
		}
		return fn(&Entry{Name: de.Name(), Path: lp, IsDir: de.IsDir(), Size: info.Size(), ModTime: info.ModTime().Unix()}, p)
	})
}

// ---- helpers ----

func (d *Driver) tmpDir() string { return filepath.Join(d.root, "tmp") }

// OpenRegular opens abs for reading only when Lstat says it is a regular
// file. Symlinks — including links pointing at files inside the same tree —
// and specials are refused WITHOUT being followed, so tree walkers that
// stream absolute paths (zip download today; shared-folder zip next) cannot
// be tricked into reading content planted via shell/rsync access. Caller
// closes the file.
func OpenRegular(abs string) (*os.File, os.FileInfo, error) {
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s: not a regular file", filepath.Base(abs))
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, err
	}
	sfi, serr := f.Stat()
	if serr != nil {
		f.Close()
		return nil, nil, serr
	}
	return f, sfi, nil
}

// newID returns a fresh random 32-hex identifier. Errors are propagated to
// callers (both call sites have error returns) instead of panicking.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func pathHash(logical string) string {
	h := sha256.Sum256([]byte(logical))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func mapStatErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist):
		return ErrNotFound
	default:
		return err
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if de.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		if !de.Type().IsRegular() {
			return nil // skip symlinks/specials
		}
		return copyFile(p, target, info.Mode())
	})
}
