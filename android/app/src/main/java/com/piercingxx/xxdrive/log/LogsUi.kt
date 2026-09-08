package com.piercingxx.xxdrive.log

import android.app.Activity
import android.app.AlertDialog
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.widget.Toast

/**
 * Copy or share the on-phone log dump. Nothing is uploaded.
 */
object LogsUi {
    fun show(activity: Activity, title: String = "Logs") {
        val dump = AppLog.shareText()
        val crash = AppLog.lastCrash()
        val preview = if (crash != null) {
            crash.take(4000)
        } else {
            dump.takeLast(4000).ifBlank { "(no lines yet)" }
        }
        AlertDialog.Builder(activity)
            .setTitle(title)
            .setMessage(preview)
            .setPositiveButton("Copy") { _, _ ->
                val clipboard = activity.getSystemService(ClipboardManager::class.java)
                clipboard?.setPrimaryClip(ClipData.newPlainText(title, dump))
                Toast.makeText(activity, "Copied", Toast.LENGTH_SHORT).show()
            }
            .setNeutralButton("Share") { _, _ ->
                val send = Intent(Intent.ACTION_SEND).apply {
                    type = "text/plain"
                    putExtra(Intent.EXTRA_SUBJECT, title)
                    putExtra(Intent.EXTRA_TEXT, dump)
                }
                activity.startActivity(Intent.createChooser(send, title))
            }
            .setNegativeButton("Done", null)
            .show()
    }
}
