package com.piercingxx.xxdrive

import android.app.Application
import com.piercingxx.xxdrive.log.AppLog

/**
 * Process-wide entry point. WorkManager can run [PhotoUploadWorker] with no
 * Activity on the stack (e.g. right after process death), so Session must be
 * initialized here — not just in Activities — before any component reads it.
 */
class XxDriveApp : Application() {
    override fun onCreate() {
        super.onCreate()
        AppLog.init(this)
        AppLog.installCrashHandler()
        AppLog.i("app", "start")
        Session.init(this)
    }
}
