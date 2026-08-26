# xx-drive for Android

WebView-based client with native bridges for file uploads/downloads and an
optional camera auto-backup worker.

## Build

1. Install [Android Studio](https://developer.android.com/studio) (Koala or newer).
2. `File → Open…` → select this `android/` directory.
3. Let Gradle sync finish, then **Run ▶** on a device/emulator, or build an APK:

```bash
cd android
./gradlew assembleDebug        # APK at app/build/outputs/apk/debug/app-debug.apk
./gradlew assembleRelease      # requires signing config for a store-ready APK
```

## First run

1. Enter your server base URL (e.g. `https://drive.example.com` or
   `http://192.168.1.10:8080` for LAN testing), username, password.
2. The app stores the bearer token locally and opens the web UI in a WebView.
3. Uploads use the native file picker; downloads go through the system
   DownloadManager into the app-private `Android/data/<pkg>/files/Download/`
   directory, named from `Content-Disposition`.

## Camera auto-backup

Settings → *Auto-upload camera photos*. New photos in MediaStore are uploaded to
`/Camera Uploads/<yyyy-MM-dd>/` every 30 minutes when the network constraint is
met (Wi-Fi only by default). Conflicts never overwrite: uploads always use the
server's `conflict=rename` mode.

## Theme sync

XX-Launcher broadcasts `xx.launcher.THEME_CHANGED` carrying a theme name and a
resolved background ARGB; xx-drive's exported receiver persists the choice and
repaints its native chrome on the next `onCreate`/`onResume`. Eight presets:
AMOLED Night, Graphite, Forest Night, Ocean Drift, Burgundy, Paper, Mist,
Custom. The nine family apps all speak this contract — set the theme once in the
launcher and the estate follows. Theme-sync, session, download names, and
backup watermark are covered by JVM unit tests.

**The WebView stays dark on every preset.** That is deliberate, not a bug: the
page inside is the server's own web UI with its own palette, and repainting
someone else's stylesheet from a broadcast is how you get unreadable text.
Native chrome themes; page content does not.

## Security notes

- **Cleartext HTTP is debug-only.** Release builds deny cleartext via
  `network_security_config.xml`; the debug source set overrides it so LAN
  `http://192.168.x.x` testing still works.
- The bearer token is stored in `EncryptedSharedPreferences`. The server URL
  stays in plain prefs so a keystore wipe does not force retyping it.
- No data is pinned offline in v1; opening files requires connectivity.

## Known limitations

- No offline pinning yet (planned via the PWA service worker + StorageManager).
- Photo backup depends on Android background-execution limits; on aggressive
  OEM builds, exempt the app from battery optimization for reliability.
