package com.piercingxx.xxdrive

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class WebViewAuthTest {

    @Test
    fun `cookie uses xxd_session name and root path`() {
        assertEquals("xxd_session=abc123; Path=/", WebViewAuth.sessionCookie("abc123"))
    }

    @Test
    fun `https origins get the Secure attribute`() {
        assertEquals(
            "xxd_session=abc123; Path=/; Secure",
            WebViewAuth.sessionCookie("abc123", secure = true),
        )
        assertTrue(WebViewAuth.secureFor("https://drive.example.com"))
        assertTrue(WebViewAuth.secureFor("HTTPS://DRIVE.EXAMPLE.COM"))
        assertFalse(WebViewAuth.secureFor("http://192.168.1.10:8080"))
    }

    @Test
    fun `cookie keeps opaque token verbatim`() {
        assertEquals(
            "xxd_session=v1.key.body.sig; Path=/",
            WebViewAuth.sessionCookie("v1.key.body.sig"),
        )
    }

    @Test
    fun `cookieUrl always ends with a single slash`() {
        assertEquals("http://192.168.1.50:8080/", WebViewAuth.cookieUrl("http://192.168.1.50:8080"))
        assertEquals("http://192.168.1.50:8080/", WebViewAuth.cookieUrl("http://192.168.1.50:8080/"))
        assertEquals("", WebViewAuth.cookieUrl("  "))
    }
}
