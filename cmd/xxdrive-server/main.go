// xxdrive-server is a self-hosted private cloud drive in a single binary.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

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
	flag.StringVar(&cfg.Addr, "addr", envOr("XXD_ADDR", ":8080"), "listen address")
	flag.StringVar(&dataDir, "data", envOr("XXD_DATA_DIR", "./data"), "data directory")
	flag.StringVar(&cfg.BaseURL, "base-url", envOr("XXD_BASE_URL", ""), "public base URL for share links")
	flag.Int64Var(&cfg.MaxUploadMB, "max-upload-mb", 10240, "max upload size in MB")
	flag.IntVar(&cfg.TrashRetentionDays, "trash-days", 30, "trash retention in days")
	flag.StringVar(&cfg.TLSCert, "tls-cert", envOr("XXD_TLS_CERT", ""), "TLS certificate path (optional)")
	flag.StringVar(&cfg.TLSKey, "tls-key", envOr("XXD_TLS_KEY", ""), "TLS key path (optional)")
	var keyringPath string
	flag.StringVar(&keyringPath, "keyring", os.Getenv(fabric.EnvKeyringPath), "fabric cluster keyring path for estate SSO (or "+fabric.EnvKeyringPath+"); optional — local admin auth works without it")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)

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

	janitorStop := startJanitor(cfg, st, fs)
	defer janitorStop()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           withWeb,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("xx-drive %s listening on %s (data: %s)", Version, cfg.Addr, dataDir)
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		log.Fatal(httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey))
	} else {
		log.Printf("WARNING: running without TLS — put behind a reverse proxy or set -tls-cert/-tls-key")
		log.Fatal(httpSrv.ListenAndServe())
	}
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
		rand.Read(b)
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

func startJanitor(cfg api.Config, st *store.Store, fs *fsdrv.Driver) func() {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		run := func() {
			cutoff := time.Now().AddDate(0, 0, -cfg.TrashRetentionDays)
			if n := fs.PurgeOldTrash(cutoff); n > 0 {
				log.Printf("janitor: purged %d expired trash items", n)
			}
			pruned, err := st.PruneVersions(32)
			if err != nil {
				log.Printf("janitor: version prune error: %v", err)
				return
			}
			for _, p := range pruned {
				os.Remove(fs.VersionBlobPath(p.Username, p.Path, p.VersionID))
			}
			// stale upload temps
			tmp := filepath.Join(fs.Root(), "tmp")
			ents, _ := os.ReadDir(tmp)
			for _, e := range ents {
				if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
					os.Remove(filepath.Join(tmp, e.Name()))
				}
			}
		}
		run()
		for {
			select {
			case <-t.C:
				run()
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
