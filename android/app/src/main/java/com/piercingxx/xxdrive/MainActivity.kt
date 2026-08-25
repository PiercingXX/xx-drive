package com.piercingxx.xxdrive

import android.annotation.SuppressLint
import android.app.DownloadManager
import android.content.Context
import android.net.Uri
import android.os.Bundle
import android.view.Menu
import android.view.MenuItem
import android.webkit.CookieManager
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity

/**
 * Main screen: hosts the server's PWA in a WebView and bridges the two things
 * a plain WebView can't do natively: file-picker uploads and authenticated downloads.
 */
class MainActivity : AppCompatActivity() {

    private lateinit var web: WebView
    private var fileUploadCallback: ValueCallback<Array<Uri>>? = null

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

        web = findViewById(R.id.webview)
        web.settings.javaScriptEnabled = true
        web.settings.domStorageEnabled = true
        web.webViewClient = WebViewClient() // stay inside the app
        web.webChromeClient = object : WebChromeClient() {
            // Bridge the web app's <input type=file> to native pickers.
            override fun onShowFileChooser(
                webView: WebView?,
                callback: ValueCallback<Array<Uri>>,
                params: FileChooserParams
            ): Boolean {
                fileUploadCallback?.onReceiveValue(null) // cancel any pending one
                fileUploadCallback = callback
                try {
                    fileChooserLauncher.launch(params.createIntent())
                } catch (e: android.content.ActivityNotFoundException) {
                    fileUploadCallback = null
                    return false
                }
                return true
            }
        }

        // Authenticated downloads via the system DownloadManager.
        web.setDownloadListener { url, _, _, mimeType, _ ->
            val req = DownloadManager.Request(Uri.parse(url)).apply {
                addRequestHeader("Authorization", "Bearer " + Session.token)
                CookieManager.getInstance().getCookie(Session.baseUrl)?.let {
                    addRequestHeader("Cookie", it)
                }
                setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
                setTitle(getString(R.string.app_name))
                setMimeType(mimeType ?: "*/*")
                setDestinationInExternalPublicDir(android.os.Environment.DIRECTORY_DOWNLOADS, "xx-drive")
            }
            getSystemService(Context.DOWNLOAD_SERVICE)?.let {
                (it as DownloadManager).enqueue(req)
            }
            Toast.makeText(this, R.string.download_started, Toast.LENGTH_SHORT).show()
        }

        web.loadUrl(Session.baseUrl + "/")
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
