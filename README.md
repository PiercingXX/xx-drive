# XX-Drive

> Private cloud like Synology Drive, minus Synology. Files stay files on disk.
> Nothing this thing does can lose them.

One Go binary on the NAS. Trash, versions, conflict copies — on every write
path, always. No PHP, no container stack, no CGO.

<img src="docs/images/screenshot.png" width="270" alt="xx-drive Android login screen on a Pixel 6, AMOLED Night">

| Piece | What |
|---|---|
| Server | Go binary. HTTP API + embedded web UI. Files on disk, SQLite for metadata. |
| Linux CLI | `xxdrive-cli` — verbs plus two-way `sync`/`watch`. For scripts. |
| Android | Kotlin client. WebView today. Compose is the destination. |
| Linux app | Not built. GTK, same look as the phone. Install with a script — Arch, Void, Alpine, Debian — not a distro of its own. |

Native Android and native Linux that look and work the same: that is the
product. Not there yet. [todo.md](todo.md).

Point the app at your server over Tailscale (or Headscale). Done.

## Server

```sh
go build -o xxdrive-server ./cmd/xxdrive-server
XXD_ADMIN_PASSWORD=secret123 ./xxdrive-server -addr 127.0.0.1:8080 -data ./data
```

Non-loopback needs TLS. [docs/MANUAL.md](docs/MANUAL.md).

```sh
go build -o xxdrive-cli ./cmd/xxdrive-cli
./xxdrive-cli login https://drive.example.com alice
./xxdrive-cli sync ~/Drive /alice
```

## Android

```sh
cd android
./gradlew assembleDebug
```

Operator smoke on a real phone: 2026-09-19.

## Status

Server: `go test -count=1 ./...` green. Android: smoketest'd. Linux GUI: not
built. Compose + GTK are still the destination.

```
Android minSdk 26
```

## License

Free and ad-free. Collects no personal data. Your files sit on your own
server, as files.
