package com.piercingxx.xxdrive

import android.content.Context
import android.content.SharedPreferences
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey

/**
 * Minimal string key-value seam over credential storage so [Session]'s
 * lifecycle rules (init-before-use, logout-keeps-baseUrl, degraded-empty
 * state) are drivable from plain JVM unit tests with an in-memory store.
 */
interface SessionStore {
    fun getString(key: String): String?
    fun putString(key: String, value: String)
    fun remove(key: String)
}

/**
 * Plain [SharedPreferences]-backed store for NON-secret values (currently only
 * the server base URL). Deliberately outside encrypted storage: recovering
 * [EncryptedSessionStore] from a broken keystore deletes its whole pref file,
 * and the server address must survive that — worst case the user re-logs in,
 * but never re-types the URL.
 */
class PlainPrefsStore(private val context: Context) : SessionStore {

    private val prefs: SharedPreferences =
        context.applicationContext.getSharedPreferences(
            Session.BASE_PREFS_FILE, Context.MODE_PRIVATE,
        )

    override fun getString(key: String): String? = prefs.getString(key, null)

    override fun putString(key: String, value: String) {
        prefs.edit().putString(key, value).apply()
    }

    override fun remove(key: String) {
        prefs.edit().remove(key).apply()
    }
}

/**
 * [EncryptedSharedPreferences]-backed store: values AES256-GCM under an
 * Android Keystore [MasterKey]. Construction THROWS when secure storage is
 * unusable; one automatic recovery attempt first deletes a corrupt or legacy
 * pref file (e.g. the wave-1 plaintext file this store replaced, or blobs
 * orphaned by a keystore change) before giving up — only the token lives here,
 * the base URL sits in [PlainPrefsStore] precisely so this deletion is cheap.
 * [Session] catches the final failure and degrades to its empty,
 * force-re-login state.
 */
class EncryptedSessionStore(private val context: Context) : SessionStore {

    private val prefs: SharedPreferences = create()

    private fun create(): SharedPreferences {
        val app = context.applicationContext
        return try {
            createEsp(app)
        } catch (first: Exception) {
            try {
                app.deleteSharedPreferences(Session.PREFS_FILE)
                createEsp(app)
            } catch (second: Exception) {
                throw IllegalStateException("secure session storage unavailable", second)
            }
        }
    }

    private fun createEsp(app: Context): SharedPreferences {
        val masterKey = MasterKey.Builder(app)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .build()
        return EncryptedSharedPreferences.create(
            app,
            Session.PREFS_FILE,
            masterKey,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    override fun getString(key: String): String? = prefs.getString(key, null)

    override fun putString(key: String, value: String) {
        prefs.edit().putString(key, value).apply()
    }

    override fun remove(key: String) {
        prefs.edit().remove(key).apply()
    }
}

/**
 * Tiny credential store. Must be initialized once ([init]) before first use —
 * Application.onCreate covers every entry path; workers defensively re-call
 * it. Storage is split: the token is encrypted ([EncryptedSessionStore]),
 * while the base URL lives in plain prefs ([PlainPrefsStore]) so keystore
 * trouble can never lose the server address. If the encrypted store cannot be
 * created the session degrades gracefully to a fresh empty state (every
 * getter empty, every write ignored → forced re-login each launch) instead of
 * crashing.
 */
object Session {
    const val PREFS_FILE = "xxdrive_session"
    const val BASE_PREFS_FILE = "xxdrive_base_url"
    const val KEY_BASE_URL = "base_url"
    const val KEY_TOKEN = "token"

    private val DEFAULT_SECURE_FACTORY: (Context) -> SessionStore = { ctx ->
        EncryptedSessionStore(ctx)
    }

    private val DEFAULT_BASE_FACTORY: (Context) -> SessionStore = { ctx ->
        PlainPrefsStore(ctx)
    }

    /** Injectable so JVM tests can drive Session over in-memory stores. */
    internal var storeFactory: (Context) -> SessionStore = DEFAULT_SECURE_FACTORY
    internal var baseStoreFactory: (Context) -> SessionStore = DEFAULT_BASE_FACTORY

    @Volatile private var store: SessionStore? = null
    @Volatile private var baseStore: SessionStore? = null

    /**
     * Idempotent per store. A failed init (null store) is retried on later
     * calls — cheap, and lets a transiently broken keystore self-heal without
     * disturbing the already-initialized plain base-URL store. Any Context
     * works ([EncryptedSessionStore] and [PlainPrefsStore] narrow to the
     * application context).
     */
    fun init(ctx: Context) {
        if (store == null) {
            store = try {
                storeFactory(ctx)
            } catch (_: Throwable) {
                null // degraded mode for the token: forced re-login
            }
        }
        if (baseStore == null) {
            baseStore = try {
                baseStoreFactory(ctx)
            } catch (_: Throwable) {
                null // degraded mode for the URL: empty prefill, writes no-op
            }
        }
    }

    /** Base URL lives in PLAIN prefs ([PlainPrefsStore]) — it is not a secret. */
    var baseUrl: String
        get() = baseStore?.getString(KEY_BASE_URL) ?: ""
        set(v) {
            baseStore?.putString(KEY_BASE_URL, v.trimEnd('/'))
        }

    var token: String
        get() = store?.getString(KEY_TOKEN) ?: ""
        set(v) {
            store?.putString(KEY_TOKEN, v)
        }

    val isLoggedIn: Boolean
        get() = SessionLogic.isLoggedIn(store != null, baseUrl, token)

    /**
     * Logout: drops the bearer token but KEEPS the last server URL so the
     * login screen can prefill it (see [LoginPrefill]).
     */
    fun clear() {
        store?.remove(KEY_TOKEN)
    }

    /** Test-only: restore the pristine uninitialized state between tests. */
    internal fun resetForTests() {
        store = null
        storeFactory = DEFAULT_SECURE_FACTORY
        baseStore = null
        baseStoreFactory = DEFAULT_BASE_FACTORY
    }
}

/** Pure prefill resolution for the login screen's server field. */
object LoginPrefill {
    /** Last stored server URL normalized for reuse; "" when nothing stored. */
    fun serverHint(storedBaseUrl: String?): String =
        storedBaseUrl?.trim()?.trimEnd('/') ?: ""
}

/** Pure login predicate, extracted so plain JVM unit tests can pin its semantics. */
object SessionLogic {
    fun isLoggedIn(initialized: Boolean, baseUrl: String, token: String): Boolean =
        initialized && baseUrl.isNotEmpty() && token.isNotEmpty()
}
