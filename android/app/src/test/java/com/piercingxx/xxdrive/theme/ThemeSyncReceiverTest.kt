package com.piercingxx.xxdrive.theme

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/** In-memory [ThemeKeyValueStore] so the receiver's decision logic is JVM-testable. */
private class InMemoryThemeKv : ThemeKeyValueStore {
    private val strings = mutableMapOf<String, String>()
    private val ints = mutableMapOf<String, Int>()

    override fun getString(key: String): String? = strings[key]
    override fun putString(key: String, value: String) {
        strings[key] = value
    }
    override fun getInt(key: String, default: Int): Int = ints[key] ?: default
    override fun putInt(key: String, value: Int) {
        ints[key] = value
    }
}

/**
 * Verifies the theme-sync receiver's two halves: the manifest wiring the OS
 * dispatches through, and the pure decision logic `onReceive` delegates to
 * (via the injectable-store seam), including the "Custom" path that honors
 * the broadcast's resolved BACKGROUND extra.
 */
class ThemeSyncReceiverTest {

    // Gradle unit tests run with the module directory (app/) as the working
    // directory; fall back to the repo-relative path for robustness.
    private val manifestText: String =
        sequenceOf(
            File("src/main/AndroidManifest.xml"),
            File("app/src/main/AndroidManifest.xml"),
        ).first { it.exists() }.readText()

    private fun store() = ThemeStore(InMemoryThemeKv())

    private fun handle(
        store: ThemeStore,
        name: String?,
        background: Int? = null,
        action: String? = ThemeSyncReceiver.ACTION_THEME_CHANGED,
    ): Boolean = ThemeSyncReceiver.handle(action, name, background, store)

    // ---- wiring: the manifest declares the receiver for the launcher broadcast ----

    @Test
    fun `manifest declares the exported theme-sync receiver for the launcher action`() {
        assertTrue(manifestText.contains(".theme.ThemeSyncReceiver"))
        assertTrue(manifestText.contains(ThemeSyncReceiver.ACTION_THEME_CHANGED))
        // The launcher is a different app: the receiver must be exported.
        val receiverBlock = manifestText.substringAfter(".theme.ThemeSyncReceiver").substringBefore("</receiver>")
        assertTrue(receiverBlock.contains("android:exported=\"true\""))
    }

    @Test
    fun `declared receiver name resolves to a class`() {
        // Throws ClassNotFoundException if the declared name is not a real class.
        Class.forName("com.piercingxx.xxdrive.theme.ThemeSyncReceiver")
    }

    @Test
    fun `contract action and extra keys`() {
        assertEquals("xx.launcher.THEME_CHANGED", ThemeSyncReceiver.ACTION_THEME_CHANGED)
        assertEquals("xx.launcher.extra.THEME_NAME", ThemeSyncReceiver.EXTRA_THEME_NAME)
        assertEquals("xx.launcher.extra.BACKGROUND", ThemeSyncReceiver.EXTRA_BACKGROUND)
    }

    // ---- decision logic: named presets ----

    @Test
    fun `a known preset name persists its key and canonical background`() {
        val s = store()
        assertTrue(handle(s, "Forest Night", background = 0xFF10261B.toInt()))
        assertEquals("forest-night", s.presetKey)
        assertEquals(0xFF10261B.toInt(), s.background)
    }

    @Test
    fun `preset name matching is case-insensitive`() {
        val s = store()
        assertTrue(handle(s, "pApEr", background = 0xFFF3EEE2.toInt()))
        assertEquals("paper", s.presetKey)
        assertEquals(0xFFF3EEE2.toInt(), s.background)
    }

    @Test
    fun `every preset round-trips through the receiver`() {
        for (preset in ThemePreset.entries) {
            val s = store()
            assertTrue(handle(s, preset.displayName, background = preset.background.toInt()))
            assertEquals(preset.key, s.presetKey)
            assertEquals(preset.background.toInt(), s.background)
        }
    }

    // ---- decision logic: Custom honors the BACKGROUND extra ----

    @Test
    fun `Custom persists the broadcast's resolved background`() {
        val s = store()
        val plum = 0xFF221024.toInt()
        assertTrue(handle(s, "Custom", background = plum))
        assertEquals(ThemeSyncReceiver.CUSTOM_KEY, s.presetKey)
        assertEquals(plum, s.background)
    }

    @Test
    fun `Custom without a background extra is ignored`() {
        val s = store()
        assertFalse(handle(s, "Custom", background = null))
        assertNull(s.presetKey)
    }

    // ---- decision logic: ignores ----

    @Test
    fun `an unknown theme name persists nothing`() {
        val s = store()
        assertFalse(handle(s, "Not A Real Preset", background = 0xFF123456.toInt()))
        assertNull(s.presetKey)
        // The store still reports the brand default ground.
        assertEquals(ThemePreset.DEFAULT.background.toInt(), s.background)
    }

    @Test
    fun `a missing theme name persists nothing`() {
        val s = store()
        assertFalse(handle(s, null, background = 0xFF123456.toInt()))
        assertNull(s.presetKey)
    }

    @Test
    fun `a wrong action persists nothing even with a valid name`() {
        val s = store()
        assertFalse(handle(s, "Graphite", background = 0xFF131316.toInt(), action = "some.other.ACTION"))
        assertFalse(handle(s, "Graphite", background = 0xFF131316.toInt(), action = null))
        assertNull(s.presetKey)
    }

    // ---- durability: a later reader over the same store sees the sync ----

    @Test
    fun `a persisted sync reaches a fresh store over the same backing kv`() {
        val kv = InMemoryThemeKv()
        assertTrue(handle(ThemeStore(kv), "Mist", background = 0xFFE6EDF5.toInt()))
        val fresh = ThemeStore(kv)
        assertEquals("mist", fresh.presetKey)
        assertEquals(0xFFE6EDF5.toInt(), fresh.background)
        // And the chrome-facing contrast rule flips to ink on this light ground.
        assertEquals(ThemeContrast.DARK_FOREGROUND, ThemeContrast.foregroundFor(fresh.background))
    }
}
