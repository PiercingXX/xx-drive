package com.piercingxx.xxdrive.theme

/**
 * Minimal key-value seam over the persistence layer so [ThemeStore] (and the
 * receiver's decision logic that writes through it) is JVM-testable with an
 * in-memory map. Production wires SharedPreferences via
 * `SharedPreferencesThemeKeyValueStore` (see ThemeSyncReceiver.kt).
 */
interface ThemeKeyValueStore {
    fun getString(key: String): String?
    fun putString(key: String, value: String)
    fun getInt(key: String, default: Int): Int
    fun putInt(key: String, value: Int)
}

/**
 * Persisted family-theme state: the launcher-reported preset key plus the
 * resolved background ARGB int. Pure Kotlin over a [ThemeKeyValueStore].
 *
 * The background int is stored separately from the key because a "Custom"
 * launcher theme has no preset entry — the broadcast's resolved BACKGROUND
 * extra is the only source of its ground.
 */
class ThemeStore(private val kv: ThemeKeyValueStore) {

    /** Persist the launcher-reported theme: [presetKey] plus resolved [background]. */
    fun save(presetKey: String, background: Int) {
        kv.putString(KEY_PRESET, presetKey)
        kv.putInt(KEY_BACKGROUND, background)
    }

    /** The persisted preset key ("amoled-night", …, or "custom"); null before any sync. */
    val presetKey: String?
        get() = kv.getString(KEY_PRESET)

    /** The persisted background ARGB int; the brand default ground before any sync. */
    val background: Int
        get() = kv.getInt(KEY_BACKGROUND, ThemePreset.DEFAULT.background.toInt())

    companion object {
        const val KEY_PRESET = "launcher_theme_preset"
        const val KEY_BACKGROUND = "launcher_theme_background"
    }
}
