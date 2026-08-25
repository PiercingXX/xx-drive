// xxdrive-cli is the Linux command-line client for xx-drive.
//
// Usage overview:
//
//	xxdrive login <url> <username>            authenticate and store credentials
//	xxdrive ls [path]                         list a remote folder
//	xxdrive mkdir <path>                      create a remote folder
//	xxdrive rm <path>                         move a remote item to trash
//	xxdrive mv <path> <destDir>               move within the drive
//	xxdrive cp <path> <destDir>               copy within the drive
//	xxdrive up <localFile> <remotePath>       upload one file
//	xxdrive down <remotePath> [localFile]     download one file
//	xxdrive sync <localDir> <remoteDir>       two-way synchronize (see below)
//	xxdrive watch <localDir> <remoteDir>      sync continuously every N seconds
//
// Sync model: three-way reconcile against a stored baseline (last-synced state).
// Changes are classified local-only / remote-only / both; "both" produces
// conflict copies on each side instead of overwriting — no data loss, ever,
// matching the server's conflict-copy contract.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

type config struct {
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
}

var cfgPath string

func main() {
	logFlags := flag.NewFlagSet("global", flag.ExitOnError)
	cfgFlag := logFlags.String("config", defaultConfigPath(), "config file path")
	interval := logFlags.Int("interval", 30, "watch interval seconds")
	logFlags.Parse(os.Args[1:])
	cfgPath = *cfgFlag
	if logFlags.NArg() < 1 {
		usage()
	}
	cmd, args := logFlags.Arg(0), logFlags.Args()[1:]
	var err error
	switch cmd {
	case "login":
		err = cmdLogin(args)
	case "ls":
		err = cmdLs(args)
	case "mkdir":
		err = cmdMkdir(args)
	case "rm":
		err = cmdRm(args)
	case "mv":
		err = cmdMoveCopy(args, "/api/files/move", map[string]any{})
	case "cp":
		err = cmdMoveCopy(args, "/api/files/copy", map[string]any{})
	case "up":
		err = cmdUp(args)
	case "down":
		err = cmdDown(args)
	case "sync":
		err = cmdSync(args, false, *interval)
	case "watch":
		err = cmdSync(args, true, *interval)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`xxdrive-cli — Linux client for xx-drive

Usage:
  xxdrive [-config FILE] <command> [args]

Commands:
  login <url> <username>          Authenticate (password prompted) and save
  ls [path]                       List remote folder
  mkdir <path>                    Create remote folder
  rm <path>                       Move remote item to trash
  mv <path> <destDir>             Move remote item
  cp <path> <destDir>             Copy remote item
  up <localFile> <remotePath>     Upload a file
  down <remotePath> [localFile]   Download a file
  sync <localDir> <remoteDir>     Two-way sync once
  watch <localDir> <remoteDir>    Two-way sync continuously (--interval N)

Flags:
  -config FILE   Config path (default ~/.config/xxdrive/config.json)
  -interval N    Watch interval seconds (watch mode)
`)
	os.Exit(0)
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "xxdrive", "config.json")
}

// ---------- config ----------

func loadCfg() (*config, error) {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("not logged in (no config at %s) — run: xxdrive login <url> <user>", cfgPath)
	}
	c := &config{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, err
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c, nil
}

func saveCfg(c *config) error {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(cfgPath, raw, 0o600)
}

// ---------- HTTP helpers ----------

func doJSON(method, url, token string, payload any) (map[string]any, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	out := map[string]any{}
	json.Unmarshal(data, &out)
	return out, nil
}

type entry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size"`
	Mtime   int64  `json:"mtime"`
	Starred bool   `json:"starred"`
}

func listDir(c *config, path string) ([]entry, error) {
	out, err := doJSON("GET", c.BaseURL+"/api/files/list?path="+url.QueryEscape(path), c.Token, nil)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(out["entries"])
	var ents []entry
	json.Unmarshal(raw, &ents)
	return ents, nil
}

// walkRemote returns every entry under root (root itself excluded), BFS.
func walkRemote(c *config, root string) (map[string]entry, error) {
	out := map[string]entry{}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		ents, err := listDir(c, cur)
		if err != nil {
			if strings.Contains(err.Error(), "404") && cur == root {
				return out, nil // remote side may not exist yet
			}
			return nil, err
		}
		for _, e := range ents {
			if e.IsDir {
				queue = append(queue, e.Path)
				continue
			}
			out[e.Path] = e
		}
	}
	return out, nil
}

func urlEscape(s string) string { return url.QueryEscape(s) }

// ---------- commands ----------

func cmdLogin(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: xxdrive login <url> <username>")
	}
	base := strings.TrimRight(args[0], "/")
	user := args[1]
	fmt.Print("Password: ")
	pw, err := readPassword()
	if err != nil {
		return err
	}
	out, err := doJSON("POST", base+"/api/auth/login", "", map[string]string{
		"username": user, "password": pw,
	})
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	tok, _ := out["token"].(string)
	if tok == "" {
		return fmt.Errorf("login returned no token")
	}
	if err := saveCfg(&config{BaseURL: base, Token: tok}); err != nil {
		return err
	}
	fmt.Println("Logged in as", user, "— config saved to", cfgPath)
	return nil
}

func readPassword() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	// piped input fallback
	rd := bufio.NewReader(os.Stdin)
	line, err := rd.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func cmdLs(args []string) error {
	c, err := loadCfg()
	if err != nil {
		return err
	}
	path := "/"
	if len(args) > 0 {
		path = args[0]
	}
	ents, err := listDir(c, path)
	if err != nil {
		return err
	}
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].IsDir != ents[j].IsDir {
			return ents[i].IsDir
		}
		return ents[i].Name < ents[j].Name
	})
	for _, e := range ents {
		kind := "f"
		if e.IsDir {
			kind = "d"
		}
		fmt.Printf("%s %10d %s %s\n", kind, e.Size, time.Unix(e.Mtime, 0).Format("2006-01-02 15:04"), e.Name)
	}
	return nil
}

func cmdMkdir(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: xxdrive mkdir <path>")
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	_, err = doJSON("POST", c.BaseURL+"/api/files/mkdir", c.Token, map[string]string{"path": args[0]})
	if err == nil {
		fmt.Println("created", args[0])
	}
	return err
}

func cmdRm(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: xxdrive rm <path>")
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	_, err = doJSON("POST", c.BaseURL+"/api/files/delete", c.Token, map[string]string{"path": args[0]})
	if err == nil {
		fmt.Println("trashed", args[0])
	}
	return err
}

func cmdMoveCopy(args []string, ep string, extra map[string]any) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: xxdrive %s <path> <destDir>", strings.TrimPrefix(ep, "/api/files/"))
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	payload := map[string]any{"path": args[0], "destDir": args[1]}
	for k, v := range extra {
		payload[k] = v
	}
	_, err = doJSON("POST", c.BaseURL+ep, c.Token, payload)
	if err == nil {
		fmt.Println("done")
	}
	return err
}

func cmdUp(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: xxdrive up <localFile> <remotePath>")
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	_, err = uploadFile(c, args[0], args[1], true)
	if err == nil {
		fmt.Println("uploaded", args[0], "->", args[1])
	}
	return err
}

func cmdDown(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: xxdrive down <remotePath> [localFile]")
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	local := filepath.Base(args[0])
	if len(args) > 1 {
		local = args[1]
	}
	n, err := downloadFile(c, args[0], local)
	if err == nil {
		fmt.Printf("downloaded %s -> %s (%d bytes)\n", args[0], local, n)
	}
	return err
}

// ---------- transfer primitives ----------

func uploadFile(c *config, localPath, remotePath string, conflictRename bool) (map[string]any, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory", localPath)
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", filepath.Base(remotePath))
		if err == nil {
			_, err = io.Copy(part, f)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	url := c.BaseURL + "/api/files/upload?path=" + urlEscape(remotePath)
	if conflictRename {
		url += "&conflict=rename"
	}
	req, err := http.NewRequest("POST", url, pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Device", hostTag())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upload: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	out := map[string]any{}
	json.Unmarshal(data, &out)
	return out, nil
}

func downloadFile(c *config, remotePath, localPath string) (int64, error) {
	req, err := http.NewRequest("GET", c.BaseURL+"/api/files/download?path="+urlEscape(remotePath), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	tmp := localPath + ".xxpart"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, cpErr := io.Copy(out, resp.Body)
	clErr := out.Close()
	if cpErr != nil || clErr != nil {
		os.Remove(tmp)
		if cpErr != nil {
			return 0, cpErr
		}
		return 0, clErr
	}
	if mt := resp.Header.Get("X-Xxd-Mtime"); mt != "" {
		// best-effort mtime restore handled by caller via entry info
		_ = mt
	}
	if err := os.Rename(tmp, localPath); err != nil {
		return 0, err
	}
	return n, nil
}

func statRemote(c *config, path string) (*entry, error) {
	out, err := doJSON("GET", c.BaseURL+"/api/files/list?path="+urlEscape(parentDir(path)), c.Token, nil)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(out["entries"])
	var ents []entry
	json.Unmarshal(raw, &ents)
	base := filepath.Base(path)
	for _, e := range ents {
		if e.Name == base {
			return &e, nil
		}
	}
	return nil, os.ErrNotExist
}

func parentDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func hostTag() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-host"
	}
	return h
}
