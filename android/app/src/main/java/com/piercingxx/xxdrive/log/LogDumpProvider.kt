package com.piercingxx.xxdrive.log

import android.content.ContentProvider
import android.content.ContentValues
import android.database.Cursor
import android.database.MatrixCursor
import android.net.Uri
import java.io.File

class LogDumpProvider : ContentProvider() {
    override fun onCreate(): Boolean = true
    override fun query(
        uri: Uri,
        projection: Array<out String>?,
        selection: String?,
        selectionArgs: Array<out String>?,
        sortOrder: String?,
    ): Cursor {
        val cursor = MatrixCursor(arrayOf("name", "text"))
        val log = AppLog.logFile()
        val files = mutableListOf<File>()
        if (log != null) {
            files += log
            val prev = File(log.parentFile, log.name + ".1")
            if (prev.isFile) files += prev
        }
        AppLog.crashFile()?.let { if (it.isFile && it.length() > 0L) files += it }
        for (file in files.filter { it.isFile && it.length() > 0L }) {
            val text = runCatching { file.readText() }.getOrNull() ?: continue
            cursor.addRow(arrayOf(file.name, text.takeLast(200_000)))
        }
        return cursor
    }
    override fun getType(uri: Uri): String = "vnd.android.cursor.dir/vnd.piercingxx.logs"
    override fun insert(uri: Uri, values: ContentValues?): Uri? = null
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?): Int = 0
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?): Int = 0
}
