package com.piercingxx.xxdrive

import android.annotation.SuppressLint
import android.app.DownloadManager
import android.content.Context
import android.net.Uri
import android.net.http.SslError
import android.os.Bundle
import android.view.Menu
import android.view.MenuItem
import android.webkit.CookieManager
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.SslErrorHandler
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import com.piercingxx.xxdrive.theme.ThemeChrome

/**
 * Main screen: hosts the server's PWA in a WebView and bridges the two things
 * a plain WebView can't do natively: file-picker uploads and authenticated downloads.
 */
class MainActivity : AppCompatActivity() {

    private lateinit var web: WebView
    private var fileUploadCallback: ValueCallback<Array<Uri>>? = null
    private var pageLoaded = false

    private val fileChooserLauncher =
        registerForActivityResult(androidx.activity.result.contract.ActivityResultContracts.StartActivityForResult()) { result ->
            val uris = if (result.resultCode == RESULT_OK) {
                WebChromeClient.FileChooserParams.parseResult(result.resultCode, result.data)
            } else null
            fileUploadCallback?.onReceiveValue(uris ?: arrayOf())
            fileUploadCallback = null
        }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        Session.init(this)
        if (!Session.isLoggedIn) {
            startActivity(android.content.Intent(this, LoginActivity::class.java))
            finish()
            return
        }
        setContentView(R.layout.activity_main)
        ThemeChrome.apply(this)

        web = findViewById(R.id.webview)
        web.settings.javaScriptEnabled = true
        web.settings.domStorageEnabled = true
        web.webViewClient = DriveWebViewClient()
        web.webChromeClient = object : WebChromeClient() {
            override fun onShowFileChooser(
                webView: WebView?,
                callback: ValueCallback<Array<Uri>>,
                params: FileChooserParams
            ): Boolean {
                fileUploadCallback?.onReceiveValue(null)
                fileUploadCallback = callback
                try {
                    fileChooserLauncher.launch(params.createIntent())
                } catch (e: android.content.ActivityNotFoundException) {
                    fileUploadCallback = null
                    Toast.makeText(this@MainActivity, "No file picker available", Toast.LENGTH_SHORT).show()
                    return false
                }
                return true
            }
        }

        web.setDownloadListener { url, _, contentDisposition, mimeType, _ ->
            val name = DownloadNames.displayName(
                contentDisposition, url, System.currentTimeMillis(),
            )
            val req = DownloadManager.Request(Uri.parse(url)).apply {
                addRequestHeader("Authorization", "Bearer " + Session.token)
                CookieManager.getInstance().getCookie(Session.baseUrl)?.let {
                    addRequestHeader("Cookie", it)
                }
                setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
                setTitle(name)
                setMimeType(mimeType ?: "*/*")
                setDestinationInExternalPublicDir(
                    android.os.Environment.DIRECTORY_DOWNLOADS,
                    name,
                )
            }
            getSystemService(Context.DOWNLOAD_SERVICE)?.let {
                (it as DownloadManager).enqueue(req)
            }
            Toast.makeText(this, R.string.download_started, Toast.LENGTH_SHORT).show()
        }

        // CookieManager.setCookie is asynchronous. Wait for its callback
        // (with a short UI-thread fallback) before loadUrl so boot()'s first
        // GET /api/auth/me carries xxd_session. Do not intercept API calls:
        // shouldInterceptRequest + OkHttp .use{} closed the body stream, and
        // buffering would OOM large downloads.
        val cookies = CookieManager.getInstance()
        cookies.setAcceptCookie(true)
        val cookie = WebViewAuth.sessionCookie(
            Session.token,
            WebViewAuth.secureFor(Session.baseUrl),
        )
        cookies.setCookie(WebViewAuth.cookieUrl(Session.baseUrl), cookie) {
            web.post { loadDriveOnce() }
        }
        web.postDelayed({ loadDriveOnce() }, 300)
    }

    private fun loadDriveOnce() {
        if (pageLoaded) return
        pageLoaded = true
        web.loadUrl(Session.baseUrl + "/")
    }

    private inner class DriveWebViewClient : WebViewClient() {
        override fun onReceivedSslError(
            view: WebView,
            handler: SslErrorHandler,
            error: SslError,
        ) {
            handler.cancel()
            showInlineError(view, WebViewErrorPage.sslError(view.url ?: Session.baseUrl, sslReason(error)))
        }

        override fun onReceivedError(
            view: WebView,
            request: WebResourceRequest,
            error: android.webkit.WebResourceError,
        ) {
            if (!request.isForMainFrame) return
            showInlineError(
                view,
                WebViewErrorPage.loadError(request.url.toString(), error.description?.toString().orEmpty()),
            )
        }

        override fun onReceivedHttpError(
            view: WebView,
            request: WebResourceRequest,
            response: WebResourceResponse,
        ) {
            if (!request.isForMainFrame) return
            showInlineError(view, WebViewErrorPage.httpError(request.url.toString(), response.statusCode))
        }
    }

    private fun showInlineError(view: WebView, html: String) {
        view.loadDataWithBaseURL(null, html, "text/html", "utf-8", null)
    }

    private fun sslReason(error: SslError): String = when (error.primaryError) {
        SslError.SSL_UNTRUSTED -> "The server's certificate is not from a trusted authority."
        SslError.SSL_IDMISMATCH -> "The certificate does not match the server's hostname."
        SslError.SSL_EXPIRED -> "The certificate has expired."
        SslError.SSL_NOTYETVALID -> "The certificate is not yet valid."
        SslError.SSL_DATE_INVALID -> "The certificate date could not be validated."
        else -> "The certificate could not be validated."
    }

    override fun onResume() {
        super.onResume()
        ThemeChrome.apply(this)
    }

    override fun onCreateOptionsMenu(menu: Menu): Boolean {
        menuInflater.inflate(R.menu.menu_main, menu)
        return true
    }

    override fun onOptionsItemSelected(item: MenuItem): Boolean = when (item.itemId) {
        R.id.action_settings -> {
            startActivity(android.content.Intent(this, SettingsActivity::class.java))
            true
        }
        else -> super.onOptionsItemSelected(item)
    }

    @Deprecated("Deprecated in Java")
    override fun onBackPressed() {
        if (web.canGoBack()) web.goBack() else @Suppress("DEPRECATION") super.onBackPressed()
    }
}
