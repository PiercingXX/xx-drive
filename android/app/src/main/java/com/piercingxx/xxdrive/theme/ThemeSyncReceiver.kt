package com.piercingxx.xxdrive.theme

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.SharedPreferences

/** [ThemeKeyValueStore] over SharedPreferences — the production wiring. */
class SharedPreferencesThemeKeyValueStore(
    private val prefs: SharedPreferences,
) : ThemeKeyValueStore {
    override fun getString(key: String): String? = prefs.getString(key, null)
    override fun putString(key: String, value: String) {
        prefs.edit().putString(key, value).apply()
    }
    override fun getInt(key: String, default: Int): Int = prefs.getInt(key, default)
    override fun putInt(key: String, value: Int) {
        prefs.edit().putInt(key, value).apply()
    }
}

/**
 * `BroadcastReceiver` for the xx-launcher's theme-change broadcast
 * (`xx.launcher.THEME_CHANGED`), the receiver half of the family-wide theme
 * sync. The launcher targets the broadcast explicitly at this package
 * (required since Android O for manifest receivers), carrying the active
 * theme's display name and its resolved background ARGB int.
 *
 * The decision logic lives in the pure [handle] function (mirroring Txxt's
 * injectable-seams pattern): a known preset name persists that preset's key
 * and canonical background; "Custom" persists the broadcast's BACKGROUND
 * extra (the only source of a custom ground); anything else — wrong action,
 * unknown name, Custom without a background — is ignored. The store factory
 * is injectable so a JVM unit test can drive the logic over an in-memory
 * store without mocking the Android platform.
 *
 * [ThemeChrome] reads the persisted state in each activity's
 * onCreate/onResume, so the native chrome picks the new theme up the next
 * time a screen is (re)shown — no process restart needed.
 */
class ThemeSyncReceiver(
    /**
     * Builds the [ThemeStore] the receiver persists into. Defaults to the
     * app's theme SharedPreferences (the same store [ThemeChrome] reads);
     * injectable for JVM tests.
     */
    private val storeFactory: (Context) -> ThemeStore = { context ->
        ThemeStore(
            SharedPreferencesThemeKeyValueStore(
                context.getSharedPreferences(ThemeChrome.PREFS, Context.MODE_PRIVATE)
            )
        )
    },
) : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        handle(
            action = intent.action,
            themeName = intent.getStringExtra(EXTRA_THEME_NAME),
            background = if (intent.hasExtra(EXTRA_BACKGROUND)) {
                intent.getIntExtra(EXTRA_BACKGROUND, 0)
            } else {
                null
            },
            store = storeFactory(context),
        )
    }

    companion object {
        /** The xx-launcher's theme-change broadcast action. */
        const val ACTION_THEME_CHANGED = "xx.launcher.THEME_CHANGED"

        /**
         * Custom signature-level permission gating sends to this receiver —
         * declared in AndroidManifest.xml and required on the receiver there;
         * only apps signed with our cert (the xx-launcher) can hold it.
         */
        const val PERMISSION_THEME_SYNC = "com.piercingxx.xxdrive.permission.THEME_SYNC"

        /** String extra: the active theme's display name (or "Custom"). */
        const val EXTRA_THEME_NAME = "xx.launcher.extra.THEME_NAME"

        /** Int extra: the resolved background ARGB int (present even for Custom). */
        const val EXTRA_BACKGROUND = "xx.launcher.extra.BACKGROUND"

        /** Display name the launcher sends for a non-preset theme. */
        const val CUSTOM_NAME = "Custom"

        /** Preset key persisted for a custom launcher theme. */
        const val CUSTOM_KEY = "custom"

        /**
         * Pure decision logic: given the broadcast's [action], [themeName],
         * and optional [background] extra, persist the theme into [store].
         * Returns true when something was persisted, false when the broadcast
         * was ignored.
         */
        fun handle(action: String?, themeName: String?, background: Int?, store: ThemeStore): Boolean {
            if (action != ACTION_THEME_CHANGED) return false
            val preset = ThemePreset.fromDisplayName(themeName)
            when {
                preset != null ->
                    store.save(preset.key, preset.background.toInt())
                CUSTOM_NAME.equals(themeName, ignoreCase = true) && background != null ->
                    store.save(CUSTOM_KEY, background)
                else -> return false
            }
            return true
        }
    }
}
