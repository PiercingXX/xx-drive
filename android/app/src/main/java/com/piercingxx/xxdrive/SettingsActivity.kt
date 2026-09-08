package com.piercingxx.xxdrive

import android.Manifest
import android.content.Context
import android.content.SharedPreferences
import android.content.pm.PackageManager
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.os.Build
import android.os.Bundle
import android.view.View
import android.webkit.CookieManager
import android.widget.Button
import android.widget.CheckBox
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat
import com.piercingxx.xxdrive.log.LogsUi
import com.piercingxx.xxdrive.theme.ThemeChrome
import androidx.work.Constraints
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.NetworkType
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkInfo
import androidx.work.WorkManager
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import java.text.DateFormat
import java.util.Date
import java.util.concurrent.TimeUnit

/** Settings: logout and camera auto-backup toggle. */
class SettingsActivity : AppCompatActivity() {

    companion object {
        private const val REQ_MEDIA_PERMISSION = 100
    }

    // Logout invalidation only: short timeouts so a dead server can't strand
    // the user on the settings screen.
    private val logoutHttp by lazy {
        OkHttpClient.Builder()
            .connectTimeout(2, TimeUnit.SECONDS)
            .callTimeout(5, TimeUnit.SECONDS)
            .build()
    }

    private var loggingOut = false
    private var backupRunning = false
    private val connectivity by lazy {
        getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
    }
    private val networkCallback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) { runOnUiThread { bindMetered() } }
        override fun onLost(network: Network) { runOnUiThread { bindMetered() } }
        override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
            runOnUiThread { bindMetered() }
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        Session.init(this)
        setContentView(R.layout.activity_settings)
        ThemeChrome.apply(this)

        val wifiOnly = findViewById<CheckBox>(R.id.wifiOnlyCheck)
        val autoBackup = findViewById<CheckBox>(R.id.autoBackupCheck)
        val logout = findViewById<Button>(R.id.logoutBtn)
        findViewById<Button>(R.id.logsBtn).setOnClickListener { LogsUi.show(this) }

        val prefs = getSharedPreferences(PhotoUploadWorker.PREFS, MODE_PRIVATE)
        wifiOnly.isChecked = prefs.getBoolean("wifi_only", true)
        autoBackup.isChecked = prefs.getBoolean("auto_backup", false)
        bindBackupStatus(prefs)

        WorkManager.getInstance(this)
            .getWorkInfosByTagLiveData(PhotoUploadWorker.TAG)
            .observe(this) { infos ->
                backupRunning = infos.any { it.state == WorkInfo.State.RUNNING }
                bindMetered()
            }

        wifiOnly.setOnCheckedChangeListener { _, checked ->
            prefs.edit().putBoolean("wifi_only", checked).apply()
            if (autoBackup.isChecked) applyBackupSchedule(checked)
            bindMetered()
        }

        autoBackup.setOnCheckedChangeListener { _, checked ->
            if (checked && !hasMediaPermission()) {
                // Revert until the user grants access; request it now.
                autoBackup.isChecked = false
                requestMediaPermission()
                return@setOnCheckedChangeListener
            }
            prefs.edit().putBoolean("auto_backup", checked).apply()
            applyBackupSchedule(wifiOnly.isChecked)
            bindMetered()
        }

        logout.setOnClickListener {
            if (loggingOut) return@setOnClickListener
            loggingOut = true
            logout.isEnabled = false

            // Snapshot BEFORE any clearing: the request needs the token that
            // Session.clear() is about to drop.
            val base = Session.baseUrl
            val token = Session.token
            CoroutineScope(Dispatchers.IO).launch {
                // Best-effort server-side invalidation — the bearer token
                // would otherwise stay valid for up to 30 days. Failures are
                // ignored; the short timeouts bound the wait.
                LogoutApi.request(base, token)?.let { req ->
                    try {
                        logoutHttp.newCall(req).execute().use { }
                    } catch (_: Exception) {
                    }
                }
                withContext(Dispatchers.Main) { finishLogout() }
            }
        }
    }

    private fun finishLogout() {
        Session.clear()
        CookieManager.getInstance().removeAllCookies(null)
        CookieManager.getInstance().flush()
        WorkManager.getInstance(this).cancelAllWorkByTag(PhotoUploadWorker.TAG)
        startActivity(android.content.Intent(this, LoginActivity::class.java))
        finish()
    }

    override fun onRequestPermissionsResult(
        requestCode: Int,
        permissions: Array<out String>,
        grantResults: IntArray,
    ) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (requestCode != REQ_MEDIA_PERMISSION) return
        val autoBackup = findViewById<CheckBox>(R.id.autoBackupCheck)
        val prefs = getSharedPreferences(PhotoUploadWorker.PREFS, MODE_PRIVATE)
        if (grantResults.isNotEmpty() &&
            grantResults[0] == PackageManager.PERMISSION_GRANTED
        ) {
            // Grant completes what the checkbox started: persist ON + schedule now.
            prefs.edit().putBoolean("auto_backup", true).apply()
            autoBackup.isChecked = true
            applyBackupSchedule(
                findViewById<CheckBox>(R.id.wifiOnlyCheck).isChecked,
            )
            bindMetered()
        } else {
            // Keep the box off; explain why nothing was scheduled.
            Toast.makeText(this, R.string.backup_permission_denied, Toast.LENGTH_LONG).show()
        }
    }

    override fun onStart() {
        super.onStart()
        connectivity.registerDefaultNetworkCallback(networkCallback)
    }

    override fun onStop() {
        connectivity.unregisterNetworkCallback(networkCallback)
        super.onStop()
    }

    override fun onResume() {
        super.onResume()
        ThemeChrome.apply(this)
        bindBackupStatus(getSharedPreferences(PhotoUploadWorker.PREFS, MODE_PRIVATE))
    }

    private fun bindBackupStatus(prefs: SharedPreferences) {
        bindLastBackup(prefs)
        bindFailures(prefs)
        bindMetered()
    }

    private fun bindLastBackup(prefs: SharedPreferences) {
        val view = findViewById<TextView>(R.id.lastBackupText)
        val at = prefs.getLong(PhotoUploadWorker.KEY_LAST_SUCCESS_AT, 0L)
        view.text = if (at <= 0L) {
            getString(R.string.backup_never)
        } else {
            getString(
                R.string.backup_last,
                DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.SHORT).format(Date(at)),
            )
        }
    }

    private fun bindFailures(prefs: SharedPreferences) {
        val view = findViewById<TextView>(R.id.backupErrorsText)
        val failures = PhotoBackup.decodeFailures(prefs.getString(PhotoUploadWorker.KEY_LAST_FAILURES, null))
        if (failures.isEmpty()) {
            view.visibility = View.GONE
            view.text = ""
            return
        }
        val lines = failures.joinToString("\n") { f ->
            val label = f.name.ifBlank { f.uri }
            getString(R.string.backup_failure_line, label, f.message)
        }
        view.text = getString(R.string.backup_failures_header) + "\n" + lines
        view.visibility = View.VISIBLE
    }

    private fun bindMetered() {
        if (isDestroyed) return
        val view = findViewById<TextView>(R.id.backupMeteredText) ?: return
        val prefs = getSharedPreferences(PhotoUploadWorker.PREFS, MODE_PRIVATE)
        val hint = PhotoBackup.meteredHint(
            backupEnabled = prefs.getBoolean("auto_backup", false),
            running = backupRunning,
            wifiOnly = prefs.getBoolean("wifi_only", true),
            connected = networkConnected(),
            metered = connectivity.isActiveNetworkMetered,
        )
        when (hint) {
            PhotoBackup.MeteredHint.HIDDEN -> {
                view.visibility = View.GONE
                view.text = ""
            }
            PhotoBackup.MeteredHint.METERED_IN_FLIGHT -> {
                view.visibility = View.VISIBLE
                view.text = getString(R.string.backup_metered_in_flight)
            }
            PhotoBackup.MeteredHint.WAITING_UNMETERED -> {
                view.visibility = View.VISIBLE
                view.text = getString(R.string.backup_waiting_unmetered)
            }
        }
    }

    private fun networkConnected(): Boolean {
        val caps = connectivity.getNetworkCapabilities(connectivity.activeNetwork) ?: return false
        return caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
    }

    private fun applyBackupSchedule(wifiOnly: Boolean) {
        val wm = WorkManager.getInstance(this)
        if (!getSharedPreferences(PhotoUploadWorker.PREFS, MODE_PRIVATE).getBoolean("auto_backup", false)) {
            wm.cancelAllWorkByTag(PhotoUploadWorker.TAG)
            Toast.makeText(this, R.string.backup_off, Toast.LENGTH_SHORT).show()
            return
        }
        val constraints = Constraints.Builder()
            .setRequiredNetworkType(if (wifiOnly) NetworkType.UNMETERED else NetworkType.CONNECTED)
            .build()
        val req = PeriodicWorkRequestBuilder<PhotoUploadWorker>(30, TimeUnit.MINUTES)
            .setConstraints(constraints)
            .addTag(PhotoUploadWorker.TAG)
            .build()
        wm.enqueueUniquePeriodicWork("camera-backup", ExistingPeriodicWorkPolicy.UPDATE, req)
        Toast.makeText(this, R.string.backup_on, Toast.LENGTH_SHORT).show()
    }

    private fun hasMediaPermission(): Boolean =
        if (Build.VERSION.SDK_INT >= 33) {
            ContextCompat.checkSelfPermission(this, Manifest.permission.READ_MEDIA_IMAGES) ==
                PackageManager.PERMISSION_GRANTED
        } else {
            ContextCompat.checkSelfPermission(this, Manifest.permission.READ_EXTERNAL_STORAGE) ==
                PackageManager.PERMISSION_GRANTED
        }

    private fun requestMediaPermission() {
        val perm = if (Build.VERSION.SDK_INT >= 33) Manifest.permission.READ_MEDIA_IMAGES
        else Manifest.permission.READ_EXTERNAL_STORAGE
        ActivityCompat.requestPermissions(this, arrayOf(perm), REQ_MEDIA_PERMISSION)
    }
}
