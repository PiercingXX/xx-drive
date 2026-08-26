package com.piercingxx.xxdrive

/**
 * Cookie string the WebView must set on the server origin before loading the
 * SPA. The Go server reads `xxd_session` (internal/api/server.go) and treats
 * it as equal to `Authorization: Bearer`. POSTs cannot be intercepted with a
 * body, so the cookie — not a Bearer proxy — is the auth path that actually
 * has to work.
 */
object WebViewAuth {
    const val COOKIE_NAME = "xxd_session"

    fun sessionCookie(token: String, secure: Boolean = false): String {
        val base = "$COOKIE_NAME=$token; Path=/"
        return if (secure) "$base; Secure" else base
    }

    fun cookieUrl(baseUrl: String): String {
        val u = baseUrl.trim().trimEnd('/')
        return if (u.isEmpty()) "" else "$u/"
    }

    fun secureFor(baseUrl: String): Boolean =
        baseUrl.startsWith("https://", ignoreCase = true)
}
