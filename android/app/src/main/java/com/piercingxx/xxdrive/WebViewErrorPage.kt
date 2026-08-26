package com.piercingxx.xxdrive

/**
 * Inline error pages for main-frame WebView failures (SSL errors, load
 * errors, HTTP error statuses). Pure HTML builder so JVM unit tests can pin
 * the escaping contract: failure URLs come off the wire and MUST be inert.
 *
 * Retry is an absolute link back to the failing origin rather than
 * location.reload(): the page is served via loadDataWithBaseURL(null, …),
 * whose opaque origin makes reload() meaningless.
 */
object WebViewErrorPage {

    /**
     * Escape for BOTH element-text and double-quoted-attribute contexts.
     * & is replaced first so entities in the input are double-escaped.
     */
    fun escape(raw: String): String = raw
        .replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
        .replace("\"", "&quot;")
        .replace("'", "&#39;")

    /** Certificate validation failure — never proceed past these. */
    fun sslError(url: String, reason: String): String =
        page("Secure connection failed", escape(reason), url)

    /** Network/DNS/main-frame load failure from onReceivedError. */
    fun loadError(url: String, description: String): String =
        page("Page failed to load", escape(description.ifBlank { "The page could not be loaded." }), url)

    /** Main frame answered with a 4xx/5xx status. */
    fun httpError(url: String, statusCode: Int): String =
        page("Server error", escape("The server answered HTTP $statusCode."), url)

    private fun page(title: String, messageHtml: String, retryUrl: String): String {
        val safeUrl = escape(retryUrl)
        return """
            <!DOCTYPE html>
            <html>
            <head>
            <meta charset="utf-8">
            <meta name="viewport" content="width=device-width, initial-scale=1">
            <style>
              body { background:#000; color:#fff; font-family:monospace;
                     display:flex; align-items:center; justify-content:center;
                     margin:0; height:100vh; }
              .card { max-width:28rem; padding:2rem; text-align:center; }
              h1 { font-size:1.15rem; margin:0 0 .75rem; font-weight:bold; }
              p { color:#9ca3af; font-size:.85rem; word-break:break-all; margin:0 0 1.75rem; }
              a { display:inline-block; padding:.7rem 1.4rem; border:1px solid #fff;
                  color:#fff; text-decoration:none; font-size:.85rem; }
            </style>
            </head>
            <body>
              <div class="card">
                <h1>$title</h1>
                <p>$messageHtml</p>
                <a href="$safeUrl">Retry</a>
              </div>
            </body>
            </html>
        """.trimIndent()
    }
}
