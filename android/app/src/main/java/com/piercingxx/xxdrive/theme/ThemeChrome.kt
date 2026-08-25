package com.piercingxx.xxdrive.theme

import android.app.Activity
import android.content.Context
import android.content.res.ColorStateList
import android.graphics.drawable.ColorDrawable
import android.view.View
import android.view.ViewGroup
import android.widget.Button
import android.widget.CompoundButton
import android.widget.EditText
import android.widget.TextView
import androidx.core.view.WindowCompat
import androidx.core.widget.CompoundButtonCompat
import com.piercingxx.xxdrive.R

/**
 * Applies the persisted family theme (written by [ThemeSyncReceiver]) to an
 * activity's native chrome: window background, status/navigation bar colors,
 * and light/dark system-bar icon appearance per the family contrast rule.
 * Call from each activity's onCreate (after setContentView) and onResume so a
 * theme change broadcast while the app is backgrounded takes effect the next
 * time a screen is shown.
 *
 * On the small native layouts (login, settings) it also flips text, hint, and
 * checkbox tints to the contrast-rule foreground so they stay legible on the
 * light grounds (Paper, Mist). NOTE: the WebView's page content is out of
 * scope — it keeps the server's own dark palette; only the native chrome
 * around it follows the family theme.
 */
object ThemeChrome {

    /** SharedPreferences file shared with [ThemeSyncReceiver]. */
    const val PREFS = "xxdrive_theme"

    /** The [ThemeStore] over the app's theme SharedPreferences. */
    fun store(context: Context): ThemeStore =
        ThemeStore(
            SharedPreferencesThemeKeyValueStore(
                context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            )
        )

    /** Apply the persisted ground to [activity]'s window chrome and text tree. */
    fun apply(activity: Activity) {
        val background = store(activity).background
        val darkForeground = ThemeContrast.prefersDarkForeground(background)

        val window = activity.window
        window.setBackgroundDrawable(ColorDrawable(background))
        @Suppress("DEPRECATION")
        run {
            // Deprecated on API 35 (edge-to-edge) but still the effective way
            // to color the bars on 26..34, and harmless on 35.
            window.statusBarColor = background
            window.navigationBarColor = background
        }
        val insets = WindowCompat.getInsetsController(window, window.decorView)
        insets.isAppearanceLightStatusBars = darkForeground
        insets.isAppearanceLightNavigationBars = darkForeground

        activity.findViewById<ViewGroup>(android.R.id.content)?.let {
            tintTextTree(it, ThemeContrast.foregroundFor(background))
        }
    }

    /**
     * Flip text/hint/checkbox colors in the small native layouts to the
     * contrast-rule [foreground]. Buttons keep their Material signal-on-ink
     * styling; the error text keeps its semantic red.
     */
    private fun tintTextTree(view: View, foreground: Int) {
        when {
            view is CompoundButton -> {
                view.setTextColor(foreground)
                CompoundButtonCompat.setButtonTintList(view, ColorStateList.valueOf(foreground))
            }
            view is Button -> Unit
            view is EditText -> {
                view.setTextColor(foreground)
                view.setHintTextColor(withAlpha(foreground, 0x80))
                view.backgroundTintList = ColorStateList.valueOf(foreground)
            }
            view is TextView -> if (view.id != R.id.errorText) view.setTextColor(foreground)
        }
        if (view is ViewGroup) {
            for (i in 0 until view.childCount) tintTextTree(view.getChildAt(i), foreground)
        }
    }

    private fun withAlpha(color: Int, alpha: Int): Int =
        (color and 0x00FFFFFF) or (alpha shl 24)
}
