package com.piercingxx.xxdrive

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.After
import org.junit.Test

/**
 * Drives the real [Session] singleton over injected in-memory stores to pin
 * its storage contract: init-before-use, logout drops the token but KEEPS the
 * server URL (login prefill), and a failed secure-store construction degrades
 * to a logged-out empty state WITHOUT losing the durable base URL.
 */
class SessionStoreTest {

    private class InMemorySessionStore : SessionStore {
        val map = mutableMapOf<String, String>()

        override fun getString(key: String): String? = map[key]
        override fun putString(key: String, value: String) {
            map[key] = value
        }

        override fun remove(key: String) {
            map.remove(key)
        }
    }

    @After
    fun restorePristineSession() {
        Session.resetForTests()
    }

    private fun initSessionWith(
        secure: InMemorySessionStore = InMemorySessionStore(),
        base: InMemorySessionStore = InMemorySessionStore(),
    ) {
        Session.storeFactory = { secure }
        Session.baseStoreFactory = { base }
        Session.init(android.app.Application())
    }

    @Test
    fun `uninitialized session is empty and logged out`() {
        assertFalse(Session.isLoggedIn)
        assertEquals("", Session.baseUrl)
        assertEquals("", Session.token)
    }

    @Test
    fun `init plus stored credentials means logged in`() {
        val secure = InMemorySessionStore()
        val base = InMemorySessionStore()
        initSessionWith(secure, base)
        assertFalse("no credentials yet", Session.isLoggedIn)
        Session.baseUrl = "http://192.168.1.5:8080"
        Session.token = "tok"
        assertTrue(Session.isLoggedIn)
        assertEquals("http://192.168.1.5:8080", Session.baseUrl)
    }

    @Test
    fun `base url is stored in the plain store not the encrypted one`() {
        val secure = InMemorySessionStore()
        val base = InMemorySessionStore()
        initSessionWith(secure, base)
        Session.baseUrl = "http://h:8080"
        Session.token = "tok"
        assertEquals("http://h:8080", base.map[Session.KEY_BASE_URL])
        assertEquals("token only in the secure store", "tok", secure.map[Session.KEY_TOKEN])
        assertFalse("base url must not leak into the encrypted file",
            secure.map.containsKey(Session.KEY_BASE_URL))
    }

    @Test
    fun `base url setter trims the trailing slash`() {
        val base = InMemorySessionStore()
        initSessionWith(base = base)
        Session.baseUrl = "http://h:8080/"
        assertEquals("http://h:8080", base.map[Session.KEY_BASE_URL])
    }

    @Test
    fun `logout clears the token but keeps the server url for prefill`() {
        val secure = InMemorySessionStore()
        val base = InMemorySessionStore()
        initSessionWith(secure, base)
        Session.baseUrl = "http://192.168.1.5:8080"
        Session.token = "tok"

        Session.clear()

        assertEquals("", Session.token)
        assertEquals("server URL must survive logout for login prefill",
            "http://192.168.1.5:8080", Session.baseUrl)
        assertFalse(Session.isLoggedIn)
        assertTrue("only the token key is removed from the secure store",
            secure.map.isEmpty())
        assertEquals("base url untouched by clear()",
            setOf(Session.KEY_BASE_URL), base.map.keys)
    }

    @Test
    fun `broken secure storage keeps the base url durable`() {
        // First run: healthy stores, user logs in.
        val base = InMemorySessionStore()
        initSessionWith(InMemorySessionStore(), base)
        Session.baseUrl = "http://192.168.1.5:8080"
        Session.token = "tok"
        Session.resetForTests()

        // Next launch: keystore broken (ESP recovery deletes its whole pref
        // file) — the plain-prefs base URL must survive it.
        val brokenSecure: (android.content.Context) -> SessionStore = {
            throw IllegalStateException("keystore unavailable")
        }
        Session.storeFactory = brokenSecure
        Session.baseStoreFactory = { base }
        Session.init(android.app.Application())

        assertEquals("server URL survives an unusable keystore",
            "http://192.168.1.5:8080", Session.baseUrl)
        assertEquals("", Session.token)
        assertFalse(Session.isLoggedIn)
    }

    @Test
    fun `failed storage init degrades to a fresh empty state without crashing`() {
        val broken: (android.content.Context) -> SessionStore = {
            throw IllegalStateException("storage unavailable")
        }
        Session.storeFactory = broken
        Session.baseStoreFactory = broken
        Session.init(android.app.Application())
        // Degraded mode: never logged in, reads are empty, writes no-op.
        assertFalse(Session.isLoggedIn)
        assertEquals("", Session.baseUrl)
        assertEquals("", Session.token)
        Session.baseUrl = "http://x"
        Session.token = "t"
        assertEquals("", Session.baseUrl)
        assertFalse(SessionLogic.isLoggedIn(initialized = false, baseUrl = "http://x", token = "t"))
    }

    @Test
    fun `init is idempotent - a later call cannot swap a backing store`() {
        val firstSecure = InMemorySessionStore()
        val firstBase = InMemorySessionStore()
        initSessionWith(firstSecure, firstBase)
        Session.baseUrl = "http://h"
        Session.token = "tok-1"
        initSessionWith(InMemorySessionStore(), InMemorySessionStore()) // defensive re-call
        assertEquals("tok-1", Session.token)
        assertEquals("http://h", Session.baseUrl)
    }
}
