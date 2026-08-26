package com.piercingxx.xxdrive

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/** Locks download filename resolution: CD header > URL path > fallback, always unique. */
class DownloadNamesTest {

    // ---- Content-Disposition parsing ----

    @Test
    fun `parses quoted filename with spaces and extension`() {
        assertEquals(
            "report 2024.pdf",
            DownloadNames.fromContentDisposition("attachment; filename=\"report 2024.pdf\""),
        )
    }

    @Test
    fun `parses unquoted filename`() {
        assertEquals(
            "budget.csv",
            DownloadNames.fromContentDisposition("attachment; filename=budget.csv; size=12"),
        )
    }

    @Test
    fun `rfc5987 filename-star wins over plain filename`() {
        val cd = "attachment; filename=\"fallback.bin\"; filename*=UTF-8''na%C3%AFve%20file.txt"
        assertEquals("naïve file.txt", DownloadNames.fromContentDisposition(cd))
    }

    @Test
    fun `blank or missing content disposition yields null`() {
        assertEquals(null, DownloadNames.fromContentDisposition(null))
        assertEquals(null, DownloadNames.fromContentDisposition(""))
        assertEquals(null, DownloadNames.fromContentDisposition("inline"))
    }

    // ---- URL path fallback ----

    @Test
    fun `falls back to last url path segment`() {
        assertEquals("sunset.jpg", DownloadNames.fromUrl("http://h:8080/static/Photos/sunset.jpg?token=x"))
    }

    @Test
    fun `url fallback percent-decodes the segment`() {
        assertEquals("my photo.jpeg", DownloadNames.fromUrl("https://h/d/my%20photo.jpeg"))
    }

    @Test
    fun `empty url path falls back to fixed base after sanitize`() {
        assertEquals("download", DownloadNames.pickName(null, "https://host:8080/"))
        assertEquals("download", DownloadNames.pickName(null, null))
    }

    // ---- sanitization + uniqueness pipeline ----

    @Test
    fun `sanitizer neutralizes separators and reserved characters`() {
        assertEquals("weird_name_.jpg", DownloadNames.sanitize("weird/name*.jpg"))
        assertEquals("a_b_c", DownloadNames.sanitize("a\\b|c"))
        assertEquals("download", DownloadNames.sanitize("..."))
        assertEquals("download", DownloadNames.sanitize("  "))
    }

    @Test
    fun `display name keeps original stem and extension`() {
        val out = DownloadNames.displayName(
            "attachment; filename=\"trip.png\"", null, timestampMs = 1_700_000_000_000L,
        )
        assertEquals("xxdrive-1700000000000-trip.png", out)
    }

    @Test
    fun `repeat downloads of the same file do not collide`() {
        val a = DownloadNames.displayName(null, "https://h/f/album.zip", 1_000L)
        val b = DownloadNames.displayName(null, "https://h/f/album.zip", 2_000L)
        assertTrue(a != b)
        assertEquals("xxdrive-1000-album.zip", a)
        assertEquals("xxdrive-2000-album.zip", b)
    }

    @Test
    fun `full pipeline prefers content disposition then url then fallback`() {
        assertEquals(
            "xxdrive-5-from-header.txt",
            DownloadNames.displayName("filename=from-header.txt", "https://h/url-name.txt", 5),
        )
        assertEquals(
            "xxdrive-6-url-name.txt",
            DownloadNames.displayName(null, "https://h/url-name.txt", 6),
        )
        assertEquals(
            "xxdrive-7-download",
            DownloadNames.displayName(null, "https://h/", 7),
        )
    }
}
