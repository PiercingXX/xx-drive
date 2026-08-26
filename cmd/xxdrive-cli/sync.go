package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"xxdrive/internal/fsdrv"
)

// Two-way sync engine.
//
// State: a baseline JSON next to the config, keyed by sha256 of the pair,
// mapping remote-relative paths → {size,mtime} as of the last completed sync.
//
// Reconcile per path across union(local, remote, baseline):
//   local-only changed  → overwrite the remote copy (the server snapshots the
//     previous version first, so nothing is lost); the baseline records the
//     post-upload remote entry and the local mtime is converged to it so the
//     next pass is a clean no-op
//   remote-only changed → pull (download / remote delete → move local file to
//     .xxdrive-trash, never a hard delete)
//   both changed        → conflict: keep BOTH versions — the local content is
//     parked as a conflict copy on the server, then both sides converge on the
//     canonical remote content at rp; the conflict copy is mirrored locally and
//     the reconciled state is written to the baseline so the next pass is a
//     clean no-op. Neither version is ever lost.
//   unchanged everywhere → nothing
//
// Known accepted edge: files with identical size+mtime but different bytes are
// treated as "same content" (metadata-level three-way merge; verifying content
// would require downloading every remote file). Such a pair converges without
// copies and adopts the shared metadata into the baseline.
//
// New files appear on one side only and propagate. Directories are created
// lazily via uploads' MkdirAllParents server behavior + explicit mkdir on pull.

type syncMeta struct {
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Etag  string `json:"etag,omitempty"` // canonical weak etag of the stored remote copy
}

// same reports whether two metadata snapshots describe the same stored state.
// Etag is deliberately excluded: local walks never produce one, and its
// presence in a baseline entry must not flip change detection.
func (m syncMeta) same(o syncMeta) bool { return m.Size == o.Size && m.Mtime == o.Mtime }

// withEtag stamps m with the server's canonical weak etag. The upload
// endpoint compares If-Match against exactly fsdrv.EtagOf(mtime, size); the
// sha256-flavoured ETag header on upload responses is NOT accepted there,
// which is why the baseline derives the value from the server-reported
// {mtime,size} instead of copying the response header.
func withEtag(m syncMeta) syncMeta {
	m.Etag = fsdrv.EtagOf(m.Mtime, m.Size)
	return m
}

type baseline map[string]syncMeta

func cmdSync(args []string, watch bool, intervalSecs int) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: xxdrive %s <localDir> <remoteDir>", map[bool]string{true: "watch", false: "sync"}[watch])
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	localDir, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	remoteDir := args[1]
	if !strings.HasPrefix(remoteDir, "/") {
		return fmt.Errorf("remote dir must be absolute (start with /)")
	}
	if fi, err := os.Stat(localDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("local dir %s must exist", localDir)
	}

	run := func() error { return syncOnce(c, localDir, remoteDir) }
	if !watch {
		return run()
	}
	fmt.Printf("watching %s ⇄ %s every %ds (ctrl-c to stop)\n", localDir, remoteDir, intervalSecs)
	// Single-flight: if a pass overruns the interval, skip ticks until the
	// current pass finishes instead of stacking passes.
	var inflight atomic.Bool
	for {
		if inflight.CompareAndSwap(false, true) {
			if err := run(); err != nil {
				fmt.Fprintln(os.Stderr, "sync error:", err)
			}
			inflight.Store(false)
		} else {
			fmt.Fprintln(os.Stderr, "previous sync pass still running — skipping this interval")
		}
		time.Sleep(time.Duration(intervalSecs) * time.Second)
	}
}

func basePathFor(localDir, remoteDir string) string {
	key := fmt.Sprintf("%s|%s", localDir, remoteDir)
	sum := sha256.Sum256([]byte(key)) // full fixed-length digest: no slicing, no collisions
	name := "sync-" + hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(filepath.Dir(cfgPath), name)
}

func loadBaseline(localDir, remoteDir string) baseline {
	raw, err := os.ReadFile(basePathFor(localDir, remoteDir))
	if err != nil {
		return baseline{}
	}
	b := baseline{}
	json.Unmarshal(raw, &b)
	return b
}

func saveBaseline(localDir, remoteDir string, b baseline) error {
	raw, err := json.MarshalIndent(b, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(basePathFor(localDir, remoteDir), raw, 0o600)
}

func walkLocal(root string) (map[string]syncMeta, error) {
	out := map[string]syncMeta{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".xxpart" || strings.HasPrefix(name, ".xxdrive-") {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		// Never treat interrupted-download staging files as user data.
		if strings.HasSuffix(name, ".xxpart") || strings.HasPrefix(name, ".xxpartial") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out["/"+filepath.ToSlash(rel)] = syncMeta{Size: info.Size(), Mtime: info.ModTime().Unix()}
		return nil
	})
	return out, err
}

func syncOnce(c *config, localDir, remoteDir string) error {
	base := loadBaseline(localDir, remoteDir)

	local, err := walkLocal(localDir)
	if err != nil {
		return err
	}
	remoteEntries, err := walkRemote(c, remoteDir)
	if err != nil {
		return err
	}
	// Remote listing paths include remoteDir; baseline keys are pair-relative.
	remote := map[string]syncMeta{}
	for p, e := range remoteEntries {
		rel := p
		if remoteDir != "/" {
			if !strings.HasPrefix(p, remoteDir+"/") && p != remoteDir {
				continue
			}
			rel = strings.TrimPrefix(p, remoteDir)
		}
		remote[rel] = syncMeta{Size: e.Size, Mtime: e.Mtime}
	}

	paths := map[string]bool{}
	for p := range local {
		paths[p] = true
	}
	for p := range remote {
		paths[p] = true
	}
	for p := range base {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	for _, rp := range sorted {
		l, hasL := local[rp]
		r, hasR := remote[rp]
		b, hasB := base[rp]

		lChanged := hasL && (!hasB || !l.same(b))
		lGone := !hasL && hasB
		rChanged := hasR && (!hasB || !r.same(b))
		rGone := !hasR && hasB

		switch {
		case lChanged && rChanged:
			if l.same(r) {
				// Same size+mtime → treated as same content by design
				// (metadata three-way merge; see file comment). Adopt the
				// shared metadata; make no copies.
				base[rp] = withEtag(l)
				continue
			}
			// CONFLICT: preserve BOTH versions, converge on canonical remote.
			resolveConflict(c, localDir, remoteDir, rp, r, base)

		case lChanged:
			// Local-only change wins: OVERWRITE the remote copy. The server
			// snapshots the previous version on overwrite, so nothing is lost.
			localAbs := filepath.Join(localDir, filepath.FromSlash(strings.TrimPrefix(rp, "/")))
			target := joinRemote(remoteDir, rp)
			// Optimistic concurrency: prove the remote still holds the state
			// recorded in the baseline. If another machine overwrote it since
			// the listing, the server answers 412 and BOTH versions survive
			// via the conflict path instead of a blind clobber. Baselines
			// from before this field existed carry no etag and push as before.
			ifMatch := ""
			if hasB && b.Etag != "" {
				ifMatch = b.Etag
			}
			out, err := uploadFile(c, localAbs, target, ifMatch, false)
			if err != nil {
				if strings.Contains(err.Error(), "HTTP 412") {
					fmt.Fprintf(os.Stderr, "  etag mismatch %s — remote changed elsewhere\n", rp)
					resolveConflict(c, localDir, remoteDir, rp, r, base)
					continue
				}
				fmt.Fprintf(os.Stderr, "  push failed %s: %v\n", rp, err)
				continue
			}
			meta, ok := metaFromUploadResp(out)
			if !ok {
				fmt.Fprintf(os.Stderr, "  push returned no entry for %s\n", rp)
				continue
			}
			// Converge the local mtime to the stored remote entry so the next
			// pass sees l == r == b instead of re-pushing forever.
			os.Chtimes(localAbs, time.Unix(meta.Mtime, 0), time.Unix(meta.Mtime, 0))
			base[rp] = meta
			fmt.Printf("  pushed %s\n", rp)

		case lGone && !rChanged && !rGone:
			// deleted locally, untouched remotely → delete remotely
			target := joinRemote(remoteDir, rp)
			if _, err := doJSON("POST", c.BaseURL+"/api/files/delete", c.Token, map[string]string{"path": target}); err != nil {
				fmt.Fprintf(os.Stderr, "  remote delete failed %s: %v\n", rp, err)
				continue
			}
			delete(base, rp)
			fmt.Printf("  deleted remote %s\n", rp)

		case rChanged:
			localAbs := filepath.Join(localDir, filepath.FromSlash(strings.TrimPrefix(rp, "/")))
			if err := os.MkdirAll(filepath.Dir(localAbs), 0o755); err != nil {
				return err
			}
			if err := pullTo(c, joinRemote(remoteDir, rp), localAbs); err != nil {
				fmt.Fprintf(os.Stderr, "  pull failed %s: %v\n", rp, err)
				continue
			}
			os.Chtimes(localAbs, time.Unix(r.Mtime, 0), time.Unix(r.Mtime, 0))
			os.Remove(localAbs + ".xxpart") // best-effort: stale staging file from an interrupted pull
			base[rp] = withEtag(r)
			fmt.Printf("  pulled %s\n", rp)

		case rGone && !lChanged && !lGone && hasB:
			// deleted remotely, untouched locally → move the local file into
			// .xxdrive-trash. NEVER a hard delete, and only for paths that
			// actually have a baseline entry (an empty/corrupt baseline can
			// never destroy the only copy on first sync).
			localAbs := filepath.Join(localDir, filepath.FromSlash(strings.TrimPrefix(rp, "/")))
			dst, err := trashLocal(localDir, rp)
			if err != nil {
				if !os.IsNotExist(err) {
					fmt.Fprintf(os.Stderr, "  local trash failed %s: %v\n", rp, err)
					continue
				}
				continue
			}
			delete(base, rp)
			fmt.Printf("  trashed local %s -> %s\n", localAbs, dst)

		case lGone && rGone:
			delete(base, rp)
		}
	}

	return saveBaseline(localDir, remoteDir, base)
}

// trashLocal moves a synced-local file into <localDir>/.xxdrive-trash/<stamp>/
// preserving its relative layout, instead of deleting it. walkLocal excludes
// the .xxdrive- prefix, so trashed files never re-enter reconciliation.
func trashLocal(localDir, relSlash string) (string, error) {
	src := filepath.Join(localDir, filepath.FromSlash(relSlash))
	if _, err := os.Lstat(src); err != nil {
		return "", err
	}
	stampRoot := filepath.Join(localDir, ".xxdrive-trash", time.Now().UTC().Format("20060102T150405"))
	for n := 0; n < 1000; n++ {
		stamp := stampRoot
		if n > 0 {
			stamp = fmt.Sprintf("%s-%d", stampRoot, n)
		}
		dst := filepath.Join(stamp, filepath.FromSlash(relSlash))
		if _, err := os.Lstat(dst); err == nil {
			continue // stamp collision — try the next suffix
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		return dst, os.Rename(src, dst)
	}
	return "", fmt.Errorf("could not allocate a trash stamp under %s", stampRoot)
}

// metaFromUploadResp extracts the stored remote entry ({size,mtime}) from an
// upload response so baselines record what the SERVER holds, not what the
// client sent. The etag is derived from that entry (see withEtag).
func metaFromUploadResp(out map[string]any) (syncMeta, bool) {
	raw, ok := out["entry"]
	if !ok {
		return syncMeta{}, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return syncMeta{}, false
	}
	var e entry
	if json.Unmarshal(b, &e) != nil || e.IsDir || e.Path == "" {
		return syncMeta{}, false
	}
	return withEtag(syncMeta{Size: e.Size, Mtime: e.Mtime}), true
}

// resolveConflict preserves BOTH divergent versions of rp, then converges both
// sides on the canonical remote content (package comment: "both changed").
// Shared by the explicit both-changed classification and by pushes the server
// rejected with HTTP 412 — a stale optimistic-concurrency view must be treated
// as both-changed rather than clobbering the other writer's bytes.
func resolveConflict(c *config, localDir, remoteDir, rp string, rMeta syncMeta, base baseline) {
	localAbs := filepath.Join(localDir, filepath.FromSlash(strings.TrimPrefix(rp, "/")))
	conflictRel := conflictName(rp, hostTag(), time.Now())
	confTarget := joinRemote(remoteDir, conflictRel)
	// 1) Park the local version safely on the server first. No If-Match: the
	//    target is a freshly named conflict copy that cannot pre-exist.
	out, err := uploadFile(c, localAbs, confTarget, "", false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  conflict push failed %s: %v\n", rp, err)
		return
	}
	confMeta, ok := metaFromUploadResp(out)
	if !ok {
		fmt.Fprintf(os.Stderr, "  conflict push returned no entry for %s\n", rp)
		return
	}
	// 2) Local bytes are safe server-side; converge local on the canonical
	//    remote content.
	target := joinRemote(remoteDir, rp)
	if err := pullTo(c, target, localAbs); err != nil {
		fmt.Fprintf(os.Stderr, "  conflict pull failed %s: %v\n", rp, err)
		return
	}
	// Baseline what ACTUALLY sits at the canonical path: on the 412 route
	// rMeta predates the other writer's commit.
	if fresh, serr := statRemote(c, target); serr == nil && !fresh.IsDir {
		rMeta = withEtag(syncMeta{Size: fresh.Size, Mtime: fresh.Mtime})
	}
	os.Chtimes(localAbs, time.Unix(rMeta.Mtime, 0), time.Unix(rMeta.Mtime, 0))
	// 3) Mirror the conflict copy locally too.
	copyLocal := filepath.Join(localDir, filepath.FromSlash(strings.TrimPrefix(conflictRel, "/")))
	if err := os.MkdirAll(filepath.Dir(copyLocal), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "  conflict copy mkdir failed %s: %v\n", rp, err)
		return
	}
	if err := pullTo(c, confTarget, copyLocal); err != nil {
		fmt.Fprintf(os.Stderr, "  conflict copy pull failed %s: %v\n", rp, err)
		return
	}
	os.Chtimes(copyLocal, time.Unix(confMeta.Mtime, 0), time.Unix(confMeta.Mtime, 0))
	// 4) Baseline the reconciled state → next pass is a clean no-op.
	base[rp] = rMeta
	base[conflictRel] = confMeta
	fmt.Printf("  CONFLICT %s — both versions kept\n", rp)
}

func joinRemote(remoteDir, relPath string) string {
	if remoteDir == "/" {
		return relPath
	}
	return remoteDir + relPath
}

func conflictName(path, tag string, at time.Time) string {
	i := strings.LastIndex(path, "/")
	dir, name := path[:i+1], path[i+1:]
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s%s (conflict from %s %s)%s", dir, stem, tag, at.Format("2006-01-02 15:04:05"), ext)
}

func pullTo(c *config, remotePath, localPath string) error {
	req, err := http.NewRequest("GET", c.BaseURL+"/api/files/download?path="+urlEscape(remotePath), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, remotePath)
	}
	tmp := localPath + ".xxpart"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, cpErr := io.Copy(out, resp.Body)
	clErr := out.Close()
	if cpErr != nil {
		os.Remove(tmp)
		return cpErr
	}
	if clErr != nil {
		os.Remove(tmp)
		return clErr
	}
	return os.Rename(tmp, localPath)
}
