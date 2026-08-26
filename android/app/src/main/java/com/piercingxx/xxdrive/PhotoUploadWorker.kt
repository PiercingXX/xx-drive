package com.piercingxx.xxdrive

import android.content.Context
import android.provider.MediaStore
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MultipartBody
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.asRequestBody
import java.io.File
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * Camera auto-backup: uploads new MediaStore images to /Camera Uploads/<date>/
 * using the server's conflict-safe upload endpoint. The last-sync watermark
 * advances only through the all-successful prefix of the batch, so a failure
 * stops it: the failed photo and everything after it are re-queried (and
 * retried) on the next run.
 */
class PhotoUploadWorker(appContext: Context, params: WorkerParameters) :
    CoroutineWorker(appContext, params) {

    companion object {
        const val TAG = "camera-backup"
        private const val PREFS = "xxdrive_settings"
        private const val KEY_LAST_TS = "last_photo_ts"
    }

    private val http = OkHttpClient()

    override suspend fun doWork(): Result = withContext(Dispatchers.IO) {
        // WorkManager may spin us up with no Activity alive (process death);
        // Session.init normally ran in XxDriveApp, but be defensive — it's idempotent.
        Session.init(applicationContext)
        if (!Session.isLoggedIn) return@withContext Result.success()
        val prefs = applicationContext.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val since = prefs.getLong(KEY_LAST_TS, System.currentTimeMillis() - 24 * 3600_000L)

        val images = queryNewImages(since)
        if (images.isEmpty()) return@withContext Result.success()

        val attempts = mutableListOf<PhotoBackup.Attempt>()
        val dayFmt = SimpleDateFormat("yyyy-MM-dd", Locale.US)
        for (item in images) {
            try {
                val day = dayFmt.format(Date(item.dateTaken))
                // Server creates parent folders automatically; conflict=rename never overwrites.
                val target = "/Camera Uploads/$day/${item.name}"
                uploadFile(item.path, target)
                attempts.add(PhotoBackup.Attempt(item.dateTaken, uploaded = true))
            } catch (e: Exception) {
                // Keep going through the batch. The watermark stops at the first
                // failure, so this file (and everything after it) stays above it
                // and the next run re-queries and retries it.
                attempts.add(PhotoBackup.Attempt(item.dateTaken, uploaded = false))
            }
        }
        val next = PhotoBackup.nextWatermark(since, attempts)
        if (next != since) {
            prefs.edit().putLong(KEY_LAST_TS, next).apply()
        }
        Result.success()
    }

    private data class ImageItem(val path: String, val name: String, val dateTaken: Long)

    private fun queryNewImages(since: Long): List<ImageItem> {
        val out = mutableListOf<ImageItem>()
        val proj = arrayOf(
            MediaStore.Images.Media._ID,
            MediaStore.Images.Media.DISPLAY_NAME,
            MediaStore.Images.Media.DATE_TAKEN,
        )
        applicationContext.contentResolver.query(
            MediaStore.Images.Media.EXTERNAL_CONTENT_URI,
            proj,
            "${MediaStore.Images.Media.DATE_TAKEN} > ?",
            arrayOf(since.toString()), // DATE_TAKEN is epoch milliseconds
            "${MediaStore.Images.Media.DATE_TAKEN} ASC",
        )?.use { cur ->
            val idCol = cur.getColumnIndexOrThrow(MediaStore.Images.Media._ID)
            val nameCol = cur.getColumnIndexOrThrow(MediaStore.Images.Media.DISPLAY_NAME)
            val dateCol = cur.getColumnIndexOrThrow(MediaStore.Images.Media.DATE_TAKEN)
            while (cur.moveToNext()) {
                val uri = android.content.ContentUris.withAppendedId(
                    MediaStore.Images.Media.EXTERNAL_CONTENT_URI, cur.getLong(idCol))
                out.add(ImageItem(uri.toString(), cur.getString(nameCol), cur.getLong(dateCol)))
            }
        }
        return out
    }

    @Throws(Exception::class)
    private fun uploadFile(uri: String, remotePath: String) {
        // Resolve content URI to a cached temp file so OkHttp can stream it.
        val temp = File.createTempFile("xxup", ".img", applicationContext.cacheDir)
        try {
            applicationContext.contentResolver.openInputStream(android.net.Uri.parse(uri))?.use { input ->
                temp.outputStream().use { output -> input.copyTo(output) }
            } ?: throw IllegalStateException("cannot open $uri")

            val url = "${Session.baseUrl}/api/files/upload" +
                "?path=" + java.net.URLEncoder.encode(remotePath, "UTF-8") +
                "&conflict=rename"
            val body = MultipartBody.Builder().setType(MultipartBody.FORM)
                .addFormDataPart(
                    "file", temp.name,
                    temp.asRequestBody("application/octet-stream".toMediaType()),
                ).build()
            val req = Request.Builder()
                .url(url)
                .header("Authorization", "Bearer " + Session.token)
                .header("X-Device", android.os.Build.MODEL)
                .post(body)
                .build()
            http.newCall(req).execute().use { resp ->
                resp.body?.string()
                if (!resp.isSuccessful) throw Exception("upload HTTP ${resp.code}")
            }
        } finally {
            temp.delete()
        }
    }
}
