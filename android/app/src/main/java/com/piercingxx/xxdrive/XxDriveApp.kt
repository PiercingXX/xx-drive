package com.piercingxx.xxdrive

import android.app.Application

/**
 * Process-wide entry point. WorkManager can run [PhotoUploadWorker] with no
 * Activity on the stack (e.g. right after process death), so Session must be
 * initialized here — not just in Activities — before any component reads it.
 */
class XxDriveApp : Application() {
    override fun onCreate() {
        super.onCreate()
        Session.init(this)
    }
}
