# xx-drive-todo3

Operator mill contract materialized from dispatch. Do not invent a different exam.

## Goal

Levels D/I/W/E. Rotating file + ring + last-crash.. Locked STACK=android. Land the named leftover files — a leftover [x] without those files is still open. Do not tick leftover from T1–T7 exam placeholders (first-frame stitch, CameraX JPEG-only bind). Do not collapse into T1 src/package.json. Do not reuse the parent mill contract. Do not mill cleaned/audiobookshelf-app-cleanroom. Further leftover: Every sync/auth/IO failure writes **why** (HTTP status, path, errno). Never swal; In-app log screen. Copy / share / export a file.; Crash handler writes `last-crash.txt` before death.; Linux: `~/.local/state/xx-drive/drive.log` (XDG). Same shape as Android..

### T1 — Levels D/I/W/E. Rotating file + ring + last-crash.

- files: android/app/src/main/java/com/piercingxx/xxdrive/LevelsD.kt, android/app/src/test/java/com/piercingxx/xxdrive/LevelsDTest.kt
- note: verify was a test this box writes, which closes the box without wiring anything into the app (xx-camera, xx-apps, 2026-09-14). Appended an app-level acceptance the mill cannot satisfy by writing a test: production code outside LevelsD must reference it.
- verify: ./gradlew :app:testDebugUnitTest --offline --tests LevelsDTest && grep -rn 'LevelsD' android/app/src/main --exclude=LevelsD.kt

### T2 — Every sync/auth/IO failure writes why (HTTP status, path, errno). Never swallow.

- files: android/app/src/main/java/com/piercingxx/xxdrive/EverySync.kt, android/app/src/test/java/com/piercingxx/xxdrive/EverySyncTest.kt
- note: verify was a test this box writes, which closes the box without wiring anything into the app (xx-camera, xx-apps, 2026-09-14). Appended an app-level acceptance the mill cannot satisfy by writing a test: production code outside EverySync must reference it.
- verify: ./gradlew :app:testDebugUnitTest --offline --tests EverySyncTest && grep -rn 'EverySync' android/app/src/main --exclude=EverySync.kt

### T3 — In-app log screen. Copy / share / export a file.

- files: android/app/src/main/java/com/piercingxx/xxdrive/ui/InApp.kt, android/app/src/test/java/com/piercingxx/xxdrive/ui/InAppTest.kt
- note: verify was a test this box writes, which closes the box without wiring anything into the app (xx-camera, xx-apps, 2026-09-14). Appended an app-level acceptance the mill cannot satisfy by writing a test: production code outside InApp must reference it.
- verify: ./gradlew :app:testDebugUnitTest --offline --tests InAppTest && grep -rn 'InApp' android/app/src/main --exclude=InApp.kt

### T4 — Crash handler writes `last-crash.txt` before death.

- files: android/app/src/main/java/com/piercingxx/xxdrive/CrashHandler.kt, android/app/src/test/java/com/piercingxx/xxdrive/CrashHandlerTest.kt
- note: verify was a test this box writes, which closes the box without wiring anything into the app (xx-camera, xx-apps, 2026-09-14). Appended an app-level acceptance the mill cannot satisfy by writing a test: production code outside CrashHandler must reference it.
- verify: ./gradlew :app:testDebugUnitTest --offline --tests CrashHandlerTest && grep -rn 'CrashHandler' android/app/src/main --exclude=CrashHandler.kt

### T5 — Linux: `~/.local/state/xx-drive/drive.log` (XDG). Same shape as Android.

- files: android/app/src/main/java/com/piercingxx/xxdrive/LinuxLocal.kt, android/app/src/test/java/com/piercingxx/xxdrive/LinuxLocalTest.kt
- note: verify was a test this box writes, which closes the box without wiring anything into the app (xx-camera, xx-apps, 2026-09-14). Appended an app-level acceptance the mill cannot satisfy by writing a test: production code outside LinuxLocal must reference it.
- verify: ./gradlew :app:testDebugUnitTest --offline --tests LinuxLocalTest && grep -rn 'LinuxLocal' android/app/src/main --exclude=LinuxLocal.kt

### T6 — Redact: tokens, passwords, file bytes. Path names are allowed.

- files: android/app/src/main/java/com/piercingxx/xxdrive/RedactTokens.kt, android/app/src/test/java/com/piercingxx/xxdrive/RedactTokensTest.kt
- note: verify was a test this box writes, which closes the box without wiring anything into the app (xx-camera, xx-apps, 2026-09-14). Appended an app-level acceptance the mill cannot satisfy by writing a test: production code outside RedactTokens must reference it.
- verify: ./gradlew :app:testDebugUnitTest --offline --tests RedactTokensTest && grep -rn 'RedactTokens' android/app/src/main --exclude=RedactTokens.kt

## Final gate

- verify: ./android/gradlew -p android :app:testDebugUnitTest --offline
