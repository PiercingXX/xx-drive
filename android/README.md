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
   DownloadManager into `Downloads/xx-drive`.

## Camera auto-backup

Settings → *Auto-upload camera photos*. New photos in MediaStore are uploaded to
`/Camera Uploads/<yyyy-MM-dd>/` every 30 minutes when the network constraint is
met (Wi-Fi only by default). Conflicts never overwrite: uploads always use the
server's `conflict=rename` mode.

## Security notes

- **Cleartext HTTP is enabled** (`android:usesCleartextTraffic="true"`) so you
  can test against LAN servers without TLS. For production, serve xx-drive over
  HTTPS and remove that attribute from `AndroidManifest.xml`.
- The token is stored in plain `SharedPreferences`. Hardening step: switch
  `Session.kt` to `EncryptedSharedPreferences` with a `MasterKey`.
- No data is pinned offline in v1; opening files requires connectivity.

## Known limitations

- No offline pinning yet (planned via the PWA service worker + StorageManager).
- Photo backup depends on Android background-execution limits; on aggressive
  OEM builds, exempt the app from battery optimization for reliability.
