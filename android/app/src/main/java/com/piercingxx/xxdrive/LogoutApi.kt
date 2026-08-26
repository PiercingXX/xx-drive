package com.piercingxx.xxdrive

import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody

/**
 * Server-side logout: POST /api/auth/logout carrying the bearer token, so the
 * server invalidates the session instead of leaving the token valid for up to
 * 30 days after local state is cleared. Pure builder (JVM-testable); callers
 * fire it best-effort on a background thread and ignore failures.
 */
object LogoutApi {

    /** Built logout [Request], or null when either input is blank/malformed. */
    fun request(baseUrl: String, token: String): Request? {
        val base = baseUrl.trim().trimEnd('/')
        val tok = token.trim()
        if (base.isEmpty() || tok.isEmpty()) return null
        return try {
            Request.Builder()
                .url("$base/api/auth/logout")
                .header("Authorization", "Bearer $tok")
                .post(ByteArray(0).toRequestBody())
                .build()
        } catch (_: IllegalArgumentException) {
            null // OkHttp rejected the base URL (malformed authority/scheme)
        }
    }
}
