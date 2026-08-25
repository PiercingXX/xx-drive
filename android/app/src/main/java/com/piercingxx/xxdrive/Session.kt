package com.piercingxx.xxdrive

import android.content.Context
import android.content.SharedPreferences

/** Tiny credential store. Production hardening: swap for EncryptedSharedPreferences. */
object Session {
    private const val PREFS = "xxdrive_session"
    private lateinit var prefs: SharedPreferences

    /** Must be called once from Application/Activity onCreate before first use. */
    fun init(ctx: Context) {
        prefs = ctx.applicationContext.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
    }

    var baseUrl: String
        get() = prefs.getString("base_url", "") ?: ""
        set(v) = prefs.edit().putString("base_url", v.trimEnd('/')).apply()

    var token: String
        get() = prefs.getString("token", "") ?: ""
        set(v) = prefs.edit().putString("token", v).apply()

    val isLoggedIn: Boolean
        get() = this::prefs.isInitialized && baseUrl.isNotEmpty() && token.isNotEmpty()

    fun clear() = prefs.edit().clear().apply()
}
