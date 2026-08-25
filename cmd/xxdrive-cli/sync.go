package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Two-way sync engine.
//
// State: a baseline JSON next to the config, keyed by a hash of the pair,
// mapping remote-relative paths → {size,mtime} as of the last completed sync.
//
// Reconcile per path across union(local, remote, baseline):
//   local-only changed  → push (upload / local delete → remote trash)
//   remote-only changed → pull (download / remote delete → local delete)
//   both changed        → conflict: keep BOTH versions on each side
//     - upload local content under "name (conflict from HOST TIME).ext"
//     - download remote content to   "name (from server TIME).ext"
//   unchanged everywhere → nothing
//
// New files appear on one side only and propagate. Directories are created
// lazily via uploads' MkdirAllParents server behavior + explicit mkdir on pull.

type syncMeta struct {
	Size  int64 `json:"size"`
	Mtime int64 `json:"mtime"`
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
	for {
		if err := run(); err != nil {
			fmt.Fprintln(os.Stderr, "sync error:", err)
		}
		time.Sleep(time.Duration(intervalSecs) * time.Second)
	}
}

func basePathFor(localDir, remoteDir string) string {
	key := fmt.Sprintf("%s|%s", localDir, remoteDir)
	h := make([]byte, 0, len(key))
	for i := 0; i < len(key); i++ {
		h = append(h, byte(key[i]*31+byte(i)))
	}
	name := fmt.Sprintf("%x", h)[:24]
	return filepath.Join(filepath.Dir(cfgPath), "sync-"+name+".json")
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

	changed := false
	for _, rp := range sorted {
		l, hasL := local[rp]
		r, hasR := remote[rp]
		b, hasB := base[rp]

		lChanged := hasL && (!hasB || l != b)
		lGone := !hasL && hasB
		rChanged := hasR && (!hasB || r != b)
		rGone := !hasR && hasB

		switch {
		case lChanged && rChanged:
			if l == r {
				// same content by size+mtime — just update baseline
				base[rp] = l
				changed = true
				continue
			}
			// CONFLICT: preserve both sides
			localAbs := filepath.Join(localDir, filepath.FromSlash(strings.TrimPrefix(rp, "/")))
			conflictRel := conflictName(rp, hostTag(), time.Now())
			if _, err := uploadFile(c, localAbs, joinRemote(remoteDir, conflictRel), false); err != nil {
				fmt.Fprintf(os.Stderr, "  conflict push failed %s: %v\n", rp, err)
				continue
			}
			serverCopyRel := conflictName(rp, "server", time.Now())
			serverLocal := filepath.Join(localDir, filepath.FromSlash(strings.TrimPrefix(serverCopyRel, "/")))
			if err := os.MkdirAll(filepath.Dir(serverLocal), 0o755); err == nil {
				if err := pullTo(c, joinRemote(remoteDir, rp), serverLocal); err != nil {
					fmt.Fprintf(os.Stderr, "  conflict pull failed %s: %v\n", rp, err)
					continue
				}
			}
			base[rp] = r
			delete(base, conflictRel)
			delete(base, serverCopyRel)
			changed = true
			fmt.Printf("  CONFLICT %s — both versions kept\n", rp)

		case lChanged:
			localAbs := filepath.Join(localDir, filepath.FromSlash(strings.TrimPrefix(rp, "/")))
			target := joinRemote(remoteDir, rp)
			if _, err := uploadFile(c, localAbs, target, true); err != nil {
				fmt.Fprintf(os.Stderr, "  push failed %s: %v\n", rp, err)
				continue
			}
			base[rp] = l
			changed = true
			fmt.Printf("  pushed %s\n", rp)

		case lGone && !rChanged && !rGone:
			// deleted locally, untouched remotely → delete remotely
			target := joinRemote(remoteDir, rp)
			if _, err := doJSON("POST", c.BaseURL+"/api/files/delete", c.Token, map[string]string{"path": target}); err != nil {
				fmt.Fprintf(os.Stderr, "  remote delete failed %s: %v\n", rp, err)
				continue
			}
			delete(base, rp)
			changed = true
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
			base[rp] = r
			changed = true
			fmt.Printf("  pulled %s\n", rp)

		case rGone && !lChanged && !lGone:
			// deleted remotely, untouched locally → delete locally
			localAbs := filepath.Join(localDir, filepath.FromSlash(strings.TrimPrefix(rp, "/")))
			if err := os.Remove(localAbs); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "  local delete failed %s: %v\n", rp, err)
				continue
			}
			delete(base, rp)
			changed = true
			fmt.Printf("  deleted local %s\n", rp)

		case lGone && rGone:
			delete(base, rp)
			changed = true
		}
	}

	if changed {
		return saveBaseline(localDir, remoteDir, base)
	}
	return saveBaseline(localDir, remoteDir, base)
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
