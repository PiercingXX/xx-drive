package com.piercingxx.xxdrive

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * Pins the server-side logout request contract: POST /api/auth/logout with the
 * bearer token, built purely so failures never depend on Android plumbing.
 */
class LogoutApiTest {

    private val base = "http://192.168.1.50:8080"

    @Test
    fun `builds a POST to api auth logout with the bearer header`() {
        val req = LogoutApi.request(base, "tok-123")
        assertNotNull(req)
        assertEquals("POST", req!!.method)
        assertEquals("$base/api/auth/logout", req.url.toString())
        assertEquals("Bearer tok-123", req.header("Authorization"))
    }

    @Test
    fun `trailing slash on base url is normalized`() {
        val req = LogoutApi.request("$base/", "t")
        assertEquals("$base/api/auth/logout", req!!.url.toString())
    }

    @Test
    fun `whitespace-padded inputs are trimmed`() {
        val req = LogoutApi.request("  $base  ", "  t ")
        assertEquals("$base/api/auth/logout", req!!.url.toString())
        assertEquals("Bearer t", req.header("Authorization"))
    }

    @Test
    fun `blank url or blank token yields no request`() {
        assertNull(LogoutApi.request("", "t"))
        assertNull(LogoutApi.request("   ", "t"))
        assertNull(LogoutApi.request(base, ""))
        assertNull(LogoutApi.request(base, "   "))
    }

    @Test
    fun `malformed base url yields no request instead of throwing`() {
        assertNull(LogoutApi.request("http://bad host with spaces:8080", "t"))
        assertNull(LogoutApi.request("not a url", "t"))
    }

    @Test
    fun `request carries an empty body`() {
        val body = LogoutApi.request(base, "t")!!.body
        assertNotNull(body)
        assertEquals(0L, body!!.contentLength())
        assertFalse(body.isOneShot())
    }
}
