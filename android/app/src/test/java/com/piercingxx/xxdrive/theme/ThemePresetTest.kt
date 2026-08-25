package com.piercingxx.xxdrive.theme

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/** Locks the preset model: the seven grounds, name resolution, and the contrast rule. */
class ThemePresetTest {

    // ---- the seven presets and their contract backgrounds ----

    @Test
    fun `ships exactly the seven family presets with the contract grounds`() {
        assertEquals(7, ThemePreset.entries.size)
        val expected = mapOf(
            "AMOLED Night" to 0xFF000000,
            "Graphite" to 0xFF131316,
            "Forest Night" to 0xFF10261B,
            "Ocean Drift" to 0xFF0F1C2E,
            "Burgundy" to 0xFF2A1018,
            "Paper" to 0xFFF3EEE2,
            "Mist" to 0xFFE6EDF5,
        )
        assertEquals(expected, ThemePreset.entries.associate { it.displayName to it.background })
    }

    // ---- fromDisplayName: all seven, unknown, case-insensitive ----

    @Test
    fun `resolves every preset by its exact display name`() {
        for (preset in ThemePreset.entries) {
            assertEquals(preset, ThemePreset.fromDisplayName(preset.displayName))
        }
    }

    @Test
    fun `display name resolution is case-insensitive`() {
        assertEquals(ThemePreset.AMOLED_NIGHT, ThemePreset.fromDisplayName("amoled night"))
        assertEquals(ThemePreset.GRAPHITE, ThemePreset.fromDisplayName("GRAPHITE"))
        assertEquals(ThemePreset.OCEAN_DRIFT, ThemePreset.fromDisplayName("ocean drift"))
        assertEquals(ThemePreset.MIST, ThemePreset.fromDisplayName("mIsT"))
    }

    @Test
    fun `unknown or missing display names resolve to null`() {
        assertNull(ThemePreset.fromDisplayName("Not A Real Preset"))
        assertNull(ThemePreset.fromDisplayName("Custom")) // custom is not a preset
        assertNull(ThemePreset.fromDisplayName(""))
        assertNull(ThemePreset.fromDisplayName(null))
    }

    @Test
    fun `resolves every preset by key and unknown keys to null`() {
        for (preset in ThemePreset.entries) {
            assertEquals(preset, ThemePreset.fromKey(preset.key))
        }
        assertNull(ThemePreset.fromKey("neon"))
        assertNull(ThemePreset.fromKey(null))
    }

    @Test
    fun `default preset is AMOLED Night`() {
        assertEquals(ThemePreset.AMOLED_NIGHT, ThemePreset.DEFAULT)
    }

    // ---- contrast rule: luminance = 0.299r + 0.587g + 0.114b, > 182 flips dark ----

    @Test
    fun `luminance follows the family weights`() {
        assertEquals(0.0, ThemeContrast.luminance(0xFF000000.toInt()), 1e-9)
        assertEquals(255.0, ThemeContrast.luminance(0xFFFFFFFF.toInt()), 1e-9)
        // Pure red / green / blue expose each weight.
        assertEquals(0.299 * 255, ThemeContrast.luminance(0xFFFF0000.toInt()), 1e-9)
        assertEquals(0.587 * 255, ThemeContrast.luminance(0xFF00FF00.toInt()), 1e-9)
        assertEquals(0.114 * 255, ThemeContrast.luminance(0xFF0000FF.toInt()), 1e-9)
    }

    @Test
    fun `luminance exactly at 182 keeps the white foreground`() {
        // Gray 0xB6 (=182): 182 * (0.299 + 0.587 + 0.114) = 182.0 exactly —
        // not greater than the threshold, so the foreground stays white.
        val gray182 = 0xFFB6B6B6.toInt()
        assertEquals(182.0, ThemeContrast.luminance(gray182), 1e-9)
        assertFalse(ThemeContrast.prefersDarkForeground(gray182))
        assertEquals(ThemeContrast.LIGHT_FOREGROUND, ThemeContrast.foregroundFor(gray182))
    }

    @Test
    fun `luminance just above 182 flips to the dark ink foreground`() {
        val gray183 = 0xFFB7B7B7.toInt()
        assertEquals(183.0, ThemeContrast.luminance(gray183), 1e-9)
        assertTrue(ThemeContrast.prefersDarkForeground(gray183))
        assertEquals(ThemeContrast.DARK_FOREGROUND, ThemeContrast.foregroundFor(gray183))
    }

    @Test
    fun `dark presets get the white foreground, light presets the ink one`() {
        for (preset in ThemePreset.entries) {
            val fg = ThemeContrast.foregroundFor(preset.background.toInt())
            if (preset.isDark) {
                assertEquals("${preset.displayName} should keep white", ThemeContrast.LIGHT_FOREGROUND, fg)
            } else {
                assertEquals("${preset.displayName} should flip to ink", ThemeContrast.DARK_FOREGROUND, fg)
            }
        }
    }

    @Test
    fun `contract foreground constants`() {
        assertEquals(0xFF1A1A1A.toInt(), ThemeContrast.DARK_FOREGROUND)
        assertEquals(0xFFFFFFFF.toInt(), ThemeContrast.LIGHT_FOREGROUND)
    }
}
