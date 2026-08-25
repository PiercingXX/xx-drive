package com.piercingxx.xxdrive.theme

/**
 * The seven named background presets of the PiercingXX family theme, mirroring
 * the xx-launcher's set (and Txxt's `ThemePreset`). Pure Kotlin — no `android.*`
 * imports — so the model is JVM-testable without a device.
 *
 * Names and background values are the brand's own, reused verbatim so the theme
 * auto-sync broadcast from the launcher can match by display name.
 */
enum class ThemePreset(
    /** Stable identifier persisted in settings. */
    val key: String,
    /** Display name carried in the launcher broadcast, e.g. "AMOLED Night". */
    val displayName: String,
    /** Background color as a 0xAARRGGBB long. */
    val background: Long,
    /** Whether the preset is a dark theme (white foreground). */
    val isDark: Boolean,
) {
    AMOLED_NIGHT("amoled-night", "AMOLED Night", 0xFF000000, true),
    GRAPHITE("graphite", "Graphite", 0xFF131316, true),
    FOREST_NIGHT("forest-night", "Forest Night", 0xFF10261B, true),
    OCEAN_DRIFT("ocean-drift", "Ocean Drift", 0xFF0F1C2E, true),
    BURGUNDY("burgundy", "Burgundy", 0xFF2A1018, true),
    PAPER("paper", "Paper", 0xFFF3EEE2, false),
    MIST("mist", "Mist", 0xFFE6EDF5, false);

    companion object {
        /** The default preset (AMOLED Night — the brand's default ground). */
        val DEFAULT: ThemePreset = AMOLED_NIGHT

        /**
         * Resolve a preset by its stable [key]. Returns null for an unknown key
         * so callers can fall back to [DEFAULT] without throwing.
         */
        fun fromKey(key: String?): ThemePreset? =
            entries.firstOrNull { it.key == key }

        /** Resolve a preset by its display name (case-insensitive). */
        fun fromDisplayName(name: String?): ThemePreset? =
            entries.firstOrNull { it.displayName.equals(name, ignoreCase = true) }
    }
}

/**
 * The family-wide contrast rule (identical across every app in the family):
 * perceived luminance `0.299r + 0.587g + 0.114b` over the background; above 182
 * the foreground flips to near-black ink, otherwise it stays white.
 *
 * Pure Kotlin so the exact threshold is JVM-testable.
 */
object ThemeContrast {
    /** Near-black foreground used over light grounds (Paper, Mist). */
    const val DARK_FOREGROUND: Int = 0xFF1A1A1A.toInt()

    /** White foreground used over dark grounds. */
    const val LIGHT_FOREGROUND: Int = 0xFFFFFFFF.toInt()

    /** Luminance above this (strictly greater) flips to the dark foreground. */
    const val DARK_FOREGROUND_THRESHOLD: Double = 182.0

    /** Perceived luminance (0..255) of an ARGB color: 0.299r + 0.587g + 0.114b. */
    fun luminance(argb: Int): Double {
        val r = ((argb ushr 16) and 0xFF).toDouble()
        val g = ((argb ushr 8) and 0xFF).toDouble()
        val b = (argb and 0xFF).toDouble()
        return 0.299 * r + 0.587 * g + 0.114 * b
    }

    /** True when [background] is light enough to need the dark ink foreground. */
    fun prefersDarkForeground(background: Int): Boolean =
        luminance(background) > DARK_FOREGROUND_THRESHOLD

    /** The foreground color for [background] per the family contrast rule. */
    fun foregroundFor(background: Int): Int =
        if (prefersDarkForeground(background)) DARK_FOREGROUND else LIGHT_FOREGROUND
}
