package com.piercingxx.xxdrive

import org.junit.Assert.assertEquals
import org.junit.Test

/** Login screen prefill: reuse the last stored server URL without retyping. */
class LoginPrefillTest {

    @Test
    fun `stored url passes through trailing-slash trimmed`() {
        assertEquals(
            "http://192.168.1.5:8080",
            LoginPrefill.serverHint("http://192.168.1.5:8080/"),
        )
        assertEquals(
            "https://drive.example.com",
            LoginPrefill.serverHint("https://drive.example.com"),
        )
    }

    @Test
    fun `null or blank storage yields an empty hint`() {
        assertEquals("", LoginPrefill.serverHint(null))
        assertEquals("", LoginPrefill.serverHint(""))
        assertEquals("", LoginPrefill.serverHint("   "))
    }

    @Test
    fun `surrounding whitespace is stripped`() {
        assertEquals(
            "http://192.168.1.9",
            LoginPrefill.serverHint("  http://192.168.1.9/  "),
        )
    }
}
