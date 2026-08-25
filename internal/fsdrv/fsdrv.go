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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func crandRead(b []byte) (int, error) { return rand.Read(b) }

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrInvalid  = errors.New("invalid path")
	ErrNotEmpty = errors.New("directory not empty")
	ErrTooLarge = errors.New("file too large")
	ErrConflict = errors.New("conflict")
)

const versionKeep = 32

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
	userRoot := filepath.Join(d.root, "files", username)
	abs = filepath.Join(userRoot, filepath.FromSlash(logical))
	// Containment check on the lexical path.
	if abs != userRoot && !strings.HasPrefix(abs, userRoot+string(os.PathSeparator)) {
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
		if strings.HasPrefix(name, ".xxpartial") {
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
func (d *Driver) Delete(username, rel string) (id string, origPath string, err error) {
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
	id = newID()
	trashItem := filepath.Join(d.root, "trash", username, id)
	if err := os.MkdirAll(filepath.Join(d.root, "trash", username), 0o700); err != nil {
		return "", "", err
	}
	if err := os.Rename(abs, trashItem); err != nil {
		return "", "", mapStatErr(err)
	}
	meta, _ := json.Marshal(map[string]any{
		"origPath":  logical,
		"name":      NameOf(logical),
		"isDir":     fi.IsDir(),
		"deletedAt": time.Now().Unix(),
	})
	if err := os.WriteFile(trashItem+".json", meta, 0o600); err != nil {
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
		var m struct {
			OrigPath  string `json:"origPath"`
			Name      string `json:"name"`
			IsDir     bool   `json:"isDir"`
			DeletedAt int64  `json:"deletedAt"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		item := &TrashItem{ID: strings.TrimSuffix(ent.Name(), ".json"), Name: m.Name, OrigPath: m.OrigPath, IsDir: m.IsDir, DeletedAt: m.DeletedAt}
		if fi, err := os.Stat(filepath.Join(dir, item.ID)); err == nil && !item.IsDir {
			item.Size = fi.Size()
		}
		out = append(out, item)
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

// PurgeOldTrash removes trashed items older than cutoff. Returns purged count.
func (d *Driver) PurgeOldTrash(cutoff time.Time) int {
	base := filepath.Join(d.root, "trash")
	users, err := os.ReadDir(base)
	if err != nil {
		return 0
	}
	n := 0
	for _, u := range users {
		if !u.IsDir() {
			continue
		}
		ents, err := os.ReadDir(filepath.Join(base, u.Name()))
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if !strings.HasSuffix(ent.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(base, u.Name(), ent.Name()))
			if err != nil {
				continue
			}
			var m struct{ DeletedAt int64 }
			if json.Unmarshal(raw, &m) != nil || time.Unix(m.DeletedAt, 0).Before(cutoff) {
				id := strings.TrimSuffix(ent.Name(), ".json")
				if os.RemoveAll(filepath.Join(base, u.Name(), id)) == nil {
					os.Remove(filepath.Join(base, u.Name(), ent.Name()))
					n++
				}
			}
		}
	}
	return n
}

// ---- versions ----

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
	vid := newID()
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

func (d *Driver) DeleteVersion(username, rel, versionID string) error {
	if !validID(versionID) {
		return ErrInvalid
	}
	_, logical, err := d.ResolveUserPath(username, rel)
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(d.root, "versions", username, pathHash(logical), versionID))
	return mapStatErr(err)
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
	root := filepath.Join(d.root, "files", username)
	var out []*Entry
	stop := errors.New("stop")
	err := filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
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

func newID() string {
	b := make([]byte, 16)
	if _, err := crandRead(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
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
