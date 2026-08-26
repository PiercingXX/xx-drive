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
//	xxdrive up <localFile> <remotePath>       upload one file (overwrite + version)
//	xxdrive down <remotePath> [localFile]     download one file
//	xxdrive sync <localDir> <remoteDir>       two-way synchronize (see below)
//	xxdrive watch <localDir> <remoteDir>      sync continuously every N seconds
//
// Safety-net and account verbs: whoami, logout, trash, versions, share,
// search, star/starred, sessions — run `xxdrive help` for the full list.
//
// Sync model: three-way reconcile against a stored baseline (last-synced state).
// Changes are classified local-only / remote-only / both. Local-only edits
// overwrite the remote copy (the server snapshots a version first); remote-only
// edits are pulled down; "both" produces conflict copies — no data loss, ever,
// matching the server's conflict-copy contract.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
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
	case "whoami":
		err = cmdWhoami(args)
	case "logout":
		err = cmdLogout(args)
	case "trash":
		err = cmdTrash(args)
	case "versions":
		err = cmdVersions(args)
	case "share":
		err = cmdShare(args)
	case "search":
		err = cmdSearch(args)
	case "star":
		err = cmdStarToggle(args)
	case "starred":
		err = cmdStarred(args)
	case "sessions":
		err = cmdSessions(args)
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
  whoami                          Show the logged-in user
  logout                          Invalidate the session, clear stored token
  ls [path]                       List remote folder
  mkdir <path>                    Create remote folder
  rm <path>                       Move remote item to trash
  mv <path> <destDir>             Move remote item
  cp <path> <destDir>             Copy remote item
  up [--if-match ETAG] <local> <remotePath>
                                  Upload a file (overwrites; prior version kept;
                                  --if-match fails with 412 on etag mismatch)
  down <remotePath> [localFile]   Download a file
  trash ls                        List trashed items
  trash restore <trashId>         Restore a trashed item
  trash delete <trashId>          Permanently purge one trashed item
  trash empty                     Permanently empty the trash
  versions ls <path>              List stored versions of a file
  versions restore <path> <id>    Roll a file back to a version
  versions get <path> <id> [out]  Download a version (stdout if no out)
  share ls                        List active shares
  share create <path> [--no-download] [--password PW] [--expires-days N]
                                  Create a public share; prints the URL
  share revoke <tokenOrHash>      Revoke a share by token or id
  search <query>                  Search the drive by name
  star <path>                     Toggle a star on a path
  starred                         List starred items
  sessions                        List sessions (* = this one)
  sessions revoke-others          Log out every other device
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
	printEntries(ents, false)
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
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	ifMatch := fs.String("if-match", "", "overwrite only if ETAG matches the remote's current etag (HTTP 412 otherwise)")
	fs.Parse(args)
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: xxdrive up [--if-match ETAG] <localFile> <remotePath>")
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	// Default to overwrite+version: an explicit `up` replaces the remote file
	// (the server snapshots the previous content). conflict=rename is reserved
	// for camera backup and true sync conflicts.
	local, remote := fs.Arg(0), fs.Arg(1)
	_, err = uploadFile(c, local, remote, *ifMatch, false)
	if err == nil {
		fmt.Println("uploaded", local, "->", remote)
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

// ---------- account / safety-net verbs ----------

func cmdWhoami(args []string) error {
	c, err := loadCfg()
	if err != nil {
		return err
	}
	out, err := doJSON("GET", c.BaseURL+"/api/auth/me", c.Token, nil)
	if err != nil {
		return err
	}
	user, _ := out["username"].(string)
	tags := ""
	if admin, _ := out["isAdmin"].(bool); admin {
		tags += " [admin]"
	}
	if fab, _ := out["fabric"].(bool); fab {
		tags += " [sso]"
	}
	fmt.Println(user + tags)
	return nil
}

func cmdLogout(args []string) error {
	c, err := loadCfg()
	if err != nil {
		return err
	}
	// The server invalidates the session; clearing the local token must
	// happen even if the server is unreachable or the session is already dead.
	if _, err := doJSON("POST", c.BaseURL+"/api/auth/logout", c.Token, nil); err != nil {
		fmt.Fprintln(os.Stderr, "warning: server logout failed:", err)
	}
	c.Token = ""
	if err := saveCfg(c); err != nil {
		return err
	}
	fmt.Println("logged out — token cleared from", cfgPath)
	return nil
}

type trashItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	OrigPath  string `json:"origPath"`
	IsDir     bool   `json:"isDir"`
	Size      int64  `json:"size"`
	DeletedAt int64  `json:"deletedAt"`
}

func trashList(c *config) ([]trashItem, error) {
	var items []trashItem
	err := getInto(c, "/api/trash", &items)
	return items, err
}

func cmdTrash(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: xxdrive trash ls|restore <trashId>|delete <trashId>|empty")
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	switch args[0] {
	case "ls":
		items, err := trashList(c)
		if err != nil {
			return err
		}
		for _, it := range items {
			kind := "f"
			if it.IsDir {
				kind = "d"
			}
			fmt.Printf("%s %s %10d %s %s\n", it.ID, kind, it.Size,
				time.Unix(it.DeletedAt, 0).Format("2006-01-02 15:04"), it.OrigPath)
		}
	case "restore", "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: xxdrive trash %s <trashId>", args[0])
		}
		ep := "/api/trash/restore"
		if args[0] == "delete" {
			ep = "/api/trash/delete"
		}
		out, err := doJSON("POST", c.BaseURL+ep, c.Token, map[string]string{"id": args[1]})
		if err != nil {
			return err
		}
		if dest, _ := out["restoredTo"].(string); dest != "" {
			fmt.Println("restored to", dest)
		} else {
			fmt.Println("done")
		}
	case "empty":
		items, _ := trashList(c)
		if _, err := doJSON("POST", c.BaseURL+"/api/trash/empty", c.Token, nil); err != nil {
			return err
		}
		fmt.Printf("trash emptied (%d items)\n", len(items))
	default:
		return fmt.Errorf("unknown trash subcommand %q", args[0])
	}
	return nil
}

type versionRow struct {
	VersionID string `json:"versionId"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"createdAt"`
}

func cmdVersions(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: xxdrive versions ls <path>|restore <path> <versionId>|get <path> <versionId> [out]")
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	switch args[0] {
	case "ls":
		if len(args) < 2 {
			return fmt.Errorf("usage: xxdrive versions ls <path>")
		}
		var vs []versionRow
		if err := getInto(c, "/api/versions?path="+urlEscape(args[1]), &vs); err != nil {
			return err
		}
		for _, v := range vs {
			fmt.Printf("%s %10d %s\n", v.VersionID, v.Size,
				time.Unix(v.CreatedAt, 0).Format("2006-01-02 15:04"))
		}
	case "restore":
		if len(args) < 3 {
			return fmt.Errorf("usage: xxdrive versions restore <path> <versionId>")
		}
		_, err := doJSON("POST", c.BaseURL+"/api/versions/restore", c.Token,
			map[string]string{"path": args[1], "versionId": args[2]})
		if err == nil {
			fmt.Println("restored", args[1], "@", args[2])
		}
		return err
	case "get":
		if len(args) < 3 {
			return fmt.Errorf("usage: xxdrive versions get <path> <versionId> [out]")
		}
		dest := "-"
		if len(args) > 3 {
			dest = args[3]
		}
		n, err := fetchBinary(c, "/api/versions/download?path="+urlEscape(args[1])+"&versionId="+urlEscape(args[2]), dest)
		if err != nil {
			return err
		}
		if dest != "-" {
			fmt.Printf("saved %s @ %s -> %s (%d bytes)\n", args[1], args[2], dest, n)
		}
	default:
		return fmt.Errorf("unknown versions subcommand %q", args[0])
	}
	return nil
}

type shareRow struct {
	TokenHash     string `json:"tokenHash"`
	Path          string `json:"path"`
	HasPassword   bool   `json:"hasPassword"`
	AllowDownload bool   `json:"allowDownload"`
	ExpiresAt     int64  `json:"expiresAt"`
	CreatedAt     int64  `json:"createdAt"`
	URL           string `json:"url"`
}

func cmdShare(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: xxdrive share ls|create <path> [...]|revoke <tokenOrHash>")
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	switch args[0] {
	case "ls":
		var rows []shareRow
		if err := getInto(c, "/api/shares", &rows); err != nil {
			return err
		}
		for _, sh := range rows {
			u := sh.URL
			if u != "" && !strings.HasPrefix(u, "http") {
				u = c.BaseURL + u // server may report a root-relative link
			}
			flags := ""
			if sh.HasPassword {
				flags += " [password]"
			}
			if !sh.AllowDownload {
				flags += " [view-only]"
			}
			exp := "never"
			if sh.ExpiresAt > 0 {
				exp = time.Unix(sh.ExpiresAt, 0).Format("2006-01-02")
			}
			fmt.Printf("%s  %s  expires %s%s\n", u, sh.Path, exp, flags)
		}
	case "create":
		fs := flag.NewFlagSet("share create", flag.ExitOnError)
		noDl := fs.Bool("no-download", false, "view-only share (previews stream, downloads refused)")
		pw := fs.String("password", "", "require a password to open the share")
		days := fs.Int("expires-days", 0, "share auto-expires after N days (0 = never)")
		fs.Parse(args[1:])
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: xxdrive share create <path> [--no-download] [--password PW] [--expires-days N]")
		}
		payload := map[string]any{"path": fs.Arg(0)}
		if *noDl {
			payload["allowDownload"] = false
		}
		if *pw != "" {
			payload["password"] = *pw
		}
		if *days > 0 {
			payload["expiresInDays"] = *days
		}
		out, err := doJSON("POST", c.BaseURL+"/api/shares", c.Token, payload)
		if err != nil {
			return err
		}
		tok, _ := out["token"].(string)
		if tok == "" {
			return fmt.Errorf("share create returned no token")
		}
		note := ""
		if *noDl {
			note += " (view-only)"
		}
		if *pw != "" {
			note += " (password required)"
		}
		fmt.Println("shared", fs.Arg(0), "->", c.BaseURL+"/s/"+tok+note)
	case "revoke":
		if len(args) < 2 {
			return fmt.Errorf("usage: xxdrive share revoke <tokenOrHash>")
		}
		id := args[1]
		if len(id) > 16 {
			// Raw capability tokens are stored hashed; the revoke endpoint
			// matches on the first 16 hex chars of sha256(token) — the same
			// derivation the server applies when creating the share.
			sum := sha256.Sum256([]byte(id))
			id = hex.EncodeToString(sum[:])[:16]
		}
		if _, err := doJSON("DELETE", c.BaseURL+"/api/shares/"+url.PathEscape(id), c.Token, nil); err != nil {
			return err
		}
		fmt.Println("share revoked")
	default:
		return fmt.Errorf("unknown share subcommand %q", args[0])
	}
	return nil
}

func cmdSearch(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: xxdrive search <query>")
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	var ents []entry
	if err := getInto(c, "/api/search?q="+urlEscape(strings.Join(args, " ")), &ents); err != nil {
		return err
	}
	printEntries(ents, true)
	return nil
}

func cmdStarToggle(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: xxdrive star <path>")
	}
	c, err := loadCfg()
	if err != nil {
		return err
	}
	out, err := doJSON("POST", c.BaseURL+"/api/star/toggle", c.Token, map[string]string{"path": args[0]})
	if err != nil {
		return err
	}
	if on, _ := out["starred"].(bool); on {
		fmt.Println("starred", args[0])
	} else {
		fmt.Println("unstarred", args[0])
	}
	return nil
}

func cmdStarred(args []string) error {
	c, err := loadCfg()
	if err != nil {
		return err
	}
	var ents []entry
	if err := getInto(c, "/api/starred", &ents); err != nil {
		return err
	}
	printEntries(ents, true)
	return nil
}

type sessionRow struct {
	Label     string `json:"label"`
	CreatedAt int64  `json:"createdAt"`
	LastSeen  int64  `json:"lastSeen"`
	ExpiresAt int64  `json:"expiresAt"`
	Current   bool   `json:"current"`
}

func cmdSessions(args []string) error {
	c, err := loadCfg()
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "revoke-others" {
		out, err := doJSON("POST", c.BaseURL+"/api/auth/sessions/revoke-others", c.Token, nil)
		if err != nil {
			return err
		}
		n, _ := out["revoked"].(float64)
		fmt.Printf("revoked %d other session(s)\n", int(n))
		return nil
	}
	var rows []sessionRow
	if err := getInto(c, "/api/auth/sessions", &rows); err != nil {
		return err
	}
	for _, s := range rows {
		mark := " "
		if s.Current {
			mark = "*"
		}
		fmt.Printf("%s %-24s last-seen %s expires %s\n", mark, s.Label,
			time.Unix(s.LastSeen, 0).Format("2006-01-02 15:04"),
			time.Unix(s.ExpiresAt, 0).Format("2006-01-02"))
	}
	return nil
}

// ---------- array/stream response helpers ----------

// authedGet issues an authorized GET and returns the raw body. Bare JSON
// arrays (trash, versions, search, shares, sessions) cannot go through
// doJSON, which only decodes objects.
func authedGet(c *config, path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// getInto GETs path and json-decodes the body into dst.
func getInto(c *config, path string, dst any) error {
	data, err := authedGet(c, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

// fetchBinary streams an authorized GET to dest ("-": stdout), staging through
// a .xxpart file for atomic writes when dest is a real path.
func fetchBinary(c *config, path, dest string) (int64, error) {
	req, err := http.NewRequest("GET", c.BaseURL+path, nil)
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
		io.Copy(io.Discard, resp.Body)
		return 0, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	if dest == "-" {
		return io.Copy(os.Stdout, resp.Body)
	}
	tmp := dest + ".xxpart"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, cpErr := io.Copy(f, resp.Body)
	clErr := f.Close()
	if cpErr != nil || clErr != nil {
		os.Remove(tmp)
		if cpErr != nil {
			return 0, cpErr
		}
		return 0, clErr
	}
	if err := os.Rename(tmp, dest); err != nil {
		return 0, err
	}
	return n, nil
}

// printEntries renders entries in the ls table style. fullPath swaps the name
// column for the drive-relative path (search/starred results span folders).
func printEntries(ents []entry, fullPath bool) {
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].IsDir != ents[j].IsDir {
			return ents[i].IsDir
		}
		return ents[i].Path < ents[j].Path
	})
	for _, e := range ents {
		kind := "f"
		if e.IsDir {
			kind = "d"
		}
		name := e.Name
		if fullPath {
			name = e.Path
		}
		star := ""
		if e.Starred {
			star = " *"
		}
		fmt.Printf("%s %10d %s %s%s\n", kind, e.Size,
			time.Unix(e.Mtime, 0).Format("2006-01-02 15:04"), name, star)
	}
}

// ---------- transfer primitives ----------

func uploadFile(c *config, localPath, remotePath, ifMatch string, conflictRename bool) (map[string]any, error) {
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
	if ifMatch != "" {
		// Optimistic concurrency: the server compares this against its
		// current weak etag (fsdrv.EtagOf) and answers 412 on mismatch,
		// leaving the stored bytes untouched.
		req.Header.Set("If-Match", ifMatch)
	}
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
