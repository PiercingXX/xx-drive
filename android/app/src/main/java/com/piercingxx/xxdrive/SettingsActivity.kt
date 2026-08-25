package com.piercingxx.xxdrive

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.widget.Button
import android.widget.CheckBox
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat
import androidx.work.Constraints
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.NetworkType
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import java.util.concurrent.TimeUnit

/** Settings: logout and camera auto-backup toggle. */
class SettingsActivity : AppCompatActivity() {

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        Session.init(this)
        setContentView(R.layout.activity_settings)

        val wifiOnly = findViewById<CheckBox>(R.id.wifiOnlyCheck)
        val autoBackup = findViewById<CheckBox>(R.id.autoBackupCheck)
        val logout = findViewById<Button>(R.id.logoutBtn)

        val prefs = getSharedPreferences("xxdrive_settings", MODE_PRIVATE)
        wifiOnly.isChecked = prefs.getBoolean("wifi_only", true)
        autoBackup.isChecked = prefs.getBoolean("auto_backup", false)

        wifiOnly.setOnCheckedChangeListener { _, checked ->
            prefs.edit().putBoolean("wifi_only", checked).apply()
            if (autoBackup.isChecked) applyBackupSchedule(checked)
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
        }

        logout.setOnClickListener {
            Session.clear()
            WorkManager.getInstance(this).cancelAllWorkByTag(PhotoUploadWorker.TAG)
            startActivity(android.content.Intent(this, LoginActivity::class.java))
            finish()
        }
    }

    private fun applyBackupSchedule(wifiOnly: Boolean) {
        val wm = WorkManager.getInstance(this)
        if (!getSharedPreferences("xxdrive_settings", MODE_PRIVATE).getBoolean("auto_backup", false)) {
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
        ActivityCompat.requestPermissions(this, arrayOf(perm), 100)
    }
}
