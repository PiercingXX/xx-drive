package com.piercingxx.xxdrive

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Session.init required-before-use semantics. Session itself needs an Android
 * Context, so the pure predicate is tested directly plus the real object's
 * pre-init state (android.jar stubs make init un-callable on plain JVM).
 */
class SessionLogicTest {

    @Test
    fun `not logged in before init`() {
        assertFalse(Session.isLoggedIn)
    }

    @Test
    fun `uninitialized state is never logged in even with leftover strings`() {
        assertFalse(SessionLogic.isLoggedIn(initialized = false, baseUrl = "http://x", token = "t"))
    }

    @Test
    fun `init plus stored credentials means logged in`() {
        assertTrue(SessionLogic.isLoggedIn(initialized = true, baseUrl = "http://192.168.1.5:8080", token = "tok"))
    }

    @Test
    fun `initialized but missing either credential is not logged in`() {
        assertFalse(SessionLogic.isLoggedIn(initialized = true, baseUrl = "", token = "tok"))
        assertFalse(SessionLogic.isLoggedIn(initialized = true, baseUrl = "http://x", token = ""))
        assertFalse(SessionLogic.isLoggedIn(initialized = true, baseUrl = "", token = ""))
    }
}
