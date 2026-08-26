package com.piercingxx.xxdrive

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Locks the inline error-page builder: hostile URL material (which comes off
 * the wire) must be inert — escaped in both element-text and attribute
 * contexts — and every page carries a working Retry link.
 */
class WebViewErrorPageTest {

    @Test
    fun `escape neutralizes markup and quote contexts`() {
        assertEquals("&lt;script&gt;", WebViewErrorPage.escape("<script>"))
        assertEquals("a&amp;b", WebViewErrorPage.escape("a&b"))
        assertEquals("&quot;x&quot;", WebViewErrorPage.escape("\"x\""))
        assertEquals("&#39;y&#39;", WebViewErrorPage.escape("'y'"))
        // & first: pre-existing entities are double-escaped, not re-parsed.
        assertEquals("&amp;lt;", WebViewErrorPage.escape("&lt;"))
    }

    @Test
    fun `hostile script url is rendered inert inside the retry link`() {
        val evil = "http://server/p?next=<script>alert(1)</script>"
        val html = WebViewErrorPage.httpError(evil, 500)
        assertFalse("raw <script> tag leaked into the page", html.contains("<script>alert"))
        assertTrue(html.contains("&lt;script&gt;alert(1)&lt;/script&gt;"))
    }

    @Test
    fun `quotes in the url cannot break out of the href attribute`() {
        val evil = """http://server/p?a=1" onmouseover="steal()""""
        val html = WebViewErrorPage.httpError(evil, 404)
        // A raw double-quote would terminate our own href attribute — none may
        // appear before the injected text; everything arrives pre-escaped.
        assertFalse(html.contains("""a=1" onmouseover"""))
        assertTrue(html.contains("a=1&quot; onmouseover=&quot;steal()&quot;"))
    }

    @Test
    fun `http status surfaces in the message`() {
        val html = WebViewErrorPage.httpError("http://s/x", 503)
        assertTrue(html.contains("HTTP 503"))
        assertTrue(html.contains("Server error"))
    }

    @Test
    fun `ssl and load pages keep title reason and retry link`() {
        val ssl = WebViewErrorPage.sslError("http://s/", "certificate expired")
        assertTrue(ssl.contains("Secure connection failed"))
        assertTrue(ssl.contains("certificate expired"))

        val load = WebViewErrorPage.loadError("http://s/", "")
        assertTrue(load.contains("Page failed to load"))
        assertTrue(load.contains("could not be loaded")) // blank description gets default text

        for (page in listOf(ssl, load)) {
            assertTrue(page.contains("""href="http://s/""""))
            assertTrue(page.contains(">Retry</a>"))
        }
    }
}
