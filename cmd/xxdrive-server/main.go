// xxdrive-server is a self-hosted private cloud drive in a single binary.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"xxdrive/internal/api"
	"xxdrive/internal/fabric"
	"xxdrive/internal/fsdrv"
	"xxdrive/internal/store"
)

// Version is the xx-drive server version. Bumped on every deployable change.
const Version = "1.1.0"

func main() {
	var cfg api.Config
	var dataDir string
	var iKnow bool
	flag.StringVar(&cfg.Addr, "addr", envOr("XXD_ADDR", ":8080"), "listen address")
	flag.StringVar(&dataDir, "data", envOr("XXD_DATA_DIR", "./data"), "data directory")
	flag.StringVar(&cfg.BaseURL, "base-url", envOr("XXD_BASE_URL", ""), "public base URL for share links")
	flag.Int64Var(&cfg.MaxUploadMB, "max-upload-mb", 10240, "max upload size in MB")
	flag.IntVar(&cfg.TrashRetentionDays, "trash-days", 30, "trash retention in days")
	flag.StringVar(&cfg.TLSCert, "tls-cert", envOr("XXD_TLS_CERT", ""), "TLS certificate path (optional)")
	flag.StringVar(&cfg.TLSKey, "tls-key", envOr("XXD_TLS_KEY", ""), "TLS key path (optional)")
	flag.BoolVar(&cfg.SecureCookies, "secure-cookies", false, "force Secure on session/share cookies (set when TLS terminates at a reverse proxy)")
	flag.BoolVar(&iKnow, "i-know", false, "allow binding a non-loopback address without TLS (cleartext credentials)")
	var keyringPath string
	flag.StringVar(&keyringPath, "keyring", os.Getenv(fabric.EnvKeyringPath), "fabric cluster keyring path for estate SSO (or "+fabric.EnvKeyringPath+"); optional — local admin auth works without it")
	var passwdUser string
	flag.StringVar(&passwdUser, "passwd", "", "interactively set a new password for USERNAME, then exit (admin recovery; run as the service user: sudo -u xxdrive xxdrive-server -data /var/lib/xxdrive -passwd admin)")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)

	// Password recovery runs standalone against the SQLite store and exits;
	// it never binds a socket, so the TLS/loopback guard below does not apply.
	if passwdUser != "" {
		if err := runPasswd(dataDir, passwdUser); err != nil {
			log.Fatalf("passwd: %v", err)
		}
		return
	}

	tlsOn := cfg.TLSCert != "" && cfg.TLSKey != ""
	if !tlsOn && !addrIsLoopback(cfg.Addr) && !iKnow {
		log.Fatalf("refusing to listen on %s without TLS: this address is reachable from other machines, so passwords and file contents would cross the network unencrypted. Bind a loopback address (-addr 127.0.0.1:8080), configure -tls-cert/-tls-key, or pass -i-know to accept the risk.", cfg.Addr)
	}

	// Create the data dir before SQLite tries to open files inside it.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	st, err := store.Open(filepath.Join(dataDir, "xxdrive.db"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	fs, err := fsdrv.New(dataDir)
	if err != nil {
		log.Fatalf("init storage: %v", err)
	}

	bootstrapAdmin(st)

	// Estate SSO: load the shared cluster keyring if one is configured. It is
	// OPTIONAL by design — with no keyring, xx-drive serves only its local
	// admin/password auth (the operator is never locked out); with one, an
	// estate account logs in via a fabric token, exactly like xx-note.
	var ring *fabric.Keyring
	if keyringPath != "" {
		r, err := fabric.LoadKeyring(keyringPath)
		if err != nil {
			log.Fatalf("fabric keyring: %v", err)
		}
		ring = r
		log.Printf("estate SSO enabled (keyring: %s)", keyringPath)
	} else {
		log.Printf("estate SSO disabled (no %s configured) — local auth only", fabric.EnvKeyringPath)
	}

	srv := api.New(cfg, st, fs, ring)
	withWeb := srv.Handler()

	janCtx, janCancel := context.WithCancel(context.Background())
	defer janCancel()
	janDone := startJanitor(janCtx, cfg, st, fs, srv)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           withWeb,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	log.Printf("xx-drive %s listening on %s (data: %s)", Version, cfg.Addr, dataDir)
	if !tlsOn {
		log.Printf("WARNING: running without TLS — put behind a reverse proxy or set -tls-cert/-tls-key")
	}
	serveErr := make(chan error, 1)
	go func() {
		if tlsOn {
			serveErr <- httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			serveErr <- httpSrv.ListenAndServe()
		}
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
		return // listener failed/returned on its own; nothing to drain
	case <-ctx.Done():
		log.Printf("shutdown signal received; draining in-flight requests (up to 15s)")
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(drainCtx); err != nil {
		log.Printf("graceful shutdown incomplete: %v", err)
	}

	// Requests are drained; give the janitor a moment to finish its sweep so
	// it never races process teardown mid-purge.
	janCancel()
	select {
	case <-janDone:
	case <-time.After(5 * time.Second):
		log.Printf("shutdown: janitor still running; exiting anyway")
	}
}

// addrIsLoopback reports whether the host part of addr resolves to this
// machine only. An empty host (":8080") means all interfaces — NOT loopback;
// a literal 127.0.0.0/8 or ::1 address is loopback; "localhost" (any case) is
// loopback; any other name counts as loopback only when DNS resolves ALL of
// its addresses to loopback IPs. Unparseable/unresolvable addresses fail
// closed (not loopback).
func addrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port; treat the whole string as a host
	}
	if host == "" {
		return false // wildcard bind reaches every interface
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	ips, err := net.LookupHost(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// runPasswd implements `-passwd USERNAME`: open the store, prompt for a new
// password twice without echo, and upsert it with the exact same PBKDF2
// format and minimum-length rule the API enforces. Run it as the service
// user so file ownership on xxdrive.db stays correct; stopping the unit
// first avoids concurrent writers (SQLite WAL tolerates it, but why race).
func runPasswd(dataDir, username string) error {
	st, err := store.Open(filepath.Join(dataDir, "xxdrive.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	fmt.Printf("New password for %s: ", username)
	in := bufio.NewReader(os.Stdin)
	pw1, err := readPasswordQuiet(in)
	if err != nil {
		return err
	}
	fmt.Print("\nConfirm: ")
	pw2, err := readPasswordQuiet(in)
	if err != nil {
		return err
	}
	fmt.Println()
	if pw1 != pw2 {
		return fmt.Errorf("passwords do not match")
	}
	if err := setUserPassword(st, username, pw1); err != nil {
		return err
	}
	fmt.Printf("password updated for %s\n", username)
	return nil
}

// setUserPassword applies the same validation and hash format as the HTTP
// password-change path (min 8 chars, store.HashPassword's PBKDF2 string).
func setUserPassword(st *store.Store, username, password string) error {
	u, err := st.UserByName(username)
	if err != nil {
		return fmt.Errorf("no such user %q", username)
	}
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	if u.Disabled {
		fmt.Fprintf(os.Stderr, "warning: account %q is disabled; the new password will work once an admin re-enables it\n", username)
	}
	return st.SetPassword(u.ID, store.HashPassword(password))
}

// readPasswordQuiet reads a password from stdin with echo disabled when the
// input is a terminal; piped input is taken as one line (for scripting).
// One reader is shared across prompts so buffered piped lines are not lost.
func readPasswordQuiet(in *bufio.Reader) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(b), err
	}
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func bootstrapAdmin(st *store.Store) {
	n, err := st.CountUsers()
	if err != nil || n > 0 {
		return
	}
	username := os.Getenv("XXD_ADMIN_USER")
	if username == "" {
		username = "admin"
	}
	password := os.Getenv("XXD_ADMIN_PASSWORD")
	generated := false
	if password == "" {
		b := make([]byte, 18)
		if _, err := rand.Read(b); err != nil {
			log.Fatalf("generate bootstrap admin password: crypto/rand failed: %v — refusing to start with an empty or guessable password", err)
		}
		password = base64.RawURLEncoding.EncodeToString(b)
		generated = true
	}
	if _, err := st.CreateUser(username, store.HashPassword(password), true); err != nil {
		log.Fatalf("bootstrap admin: %v", err)
	}
	if generated {
		fmt.Printf("\n=== First run: created admin user ===\n  username: %s\n  password: %s\n  (shown once — change it after first login)\n\n", username, password)
	} else {
		fmt.Printf("Created admin user %q from XXD_ADMIN_PASSWORD\n", username)
	}
}

// startJanitor runs the hourly maintenance sweep until ctx is cancelled and
// returns a channel closed when the sweep goroutine has fully stopped.
func startJanitor(ctx context.Context, cfg api.Config, st *store.Store, fs *fsdrv.Driver, srv *api.Server) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		janitorLoop(ctx, time.Hour, func() { runJanitorOnce(cfg, st, fs, srv) })
	}()
	return done
}

// janitorLoop runs fn immediately, then at every tick, stopping promptly when
// ctx is cancelled. Factored out so tests can exercise the stop behavior
// without real storage.
func janitorLoop(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	fn()
	for {
		select {
		case <-t.C:
			fn()
		case <-ctx.Done():
			return
		}
	}
}

// runJanitorOnce is one maintenance sweep: expire old trash (dropping each
// purged item's version history along with it), count-prune version blobs,
// drop expired share grants, and clean stale upload temporaries.
func runJanitorOnce(cfg api.Config, st *store.Store, fs *fsdrv.Driver, srv *api.Server) {
	cutoff := time.Now().AddDate(0, 0, -cfg.TrashRetentionDays)
	purged := fs.PurgeOldTrash(cutoff)
	if len(purged) > 0 {
		log.Printf("janitor: purged %d expired trash items", len(purged))
		purgeVersionsOfPurgedTrash(st, fs, purged)
	}
	pruned, err := st.PruneVersions(32)
	if err != nil {
		log.Printf("janitor: version prune error: %v", err)
	} else {
		for _, p := range pruned {
			if err := fs.PruneVersionBlob(p.Username, p.Path, p.VersionID); err != nil {
				log.Printf("janitor: remove version blob %s/%s@%s: %v", p.Username, p.Path, p.VersionID, err)
			}
		}
	}
	srv.SweepShareGrants()
	// stale upload temps
	tmp := filepath.Join(fs.Root(), "tmp")
	ents, _ := os.ReadDir(tmp)
	for _, e := range ents {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			os.Remove(filepath.Join(tmp, e.Name()))
		}
	}
}

// purgeVersionsOfPurgedTrash drops the version index rows and blob dirs for
// every trash item the janitor just removed permanently — the same pair of
// calls the API layer makes at its interactive permanent-purge sites. Items
// whose owner no longer exists are skipped (their rows already went with the
// user row), and items whose metadata lacked origPath never had history
// attributable to them.
func purgeVersionsOfPurgedTrash(st *store.Store, fs *fsdrv.Driver, purged []*fsdrv.PurgedTrash) {
	byUser := map[string][]string{}
	for _, it := range purged {
		if it.OrigPath == "" || it.OrigPath == "/" {
			continue
		}
		byUser[it.Username] = append(byUser[it.Username], it.OrigPath)
	}
	for username, paths := range byUser {
		u, err := st.UserByName(username)
		if err != nil {
			log.Printf("janitor: version cleanup skipped for user %q: %v", username, err)
			continue
		}
		for _, p := range paths {
			dropped, err := st.DeleteVersionsUnder(u.ID, p)
			if err != nil {
				log.Printf("janitor: version index purge under %s: %v", p, err)
				continue
			}
			for _, vp := range dropped {
				if err := fs.DeleteVersionDir(username, vp); err != nil {
					log.Printf("janitor: version blobs under %s: %v", vp, err)
				}
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
