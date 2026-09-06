package com.piercingxx.xxdrive

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/**
 * Locks the watermark rule: it advances only through the all-successful PREFIX
 * of the ascending-sorted batch and stops at the first failure, so a failed
 * photo (and anything newer) is re-queried and retried next run instead of
 * being permanently skipped.
 */
class PhotoBackupTest {

    @Test
    fun `fail-first batch does not advance the watermark at all`() {
        // Advancing past a failed oldest photo would skip it forever.
        val attempts = listOf(
            PhotoBackup.Attempt(dateTaken = 100L, uploaded = false),
            PhotoBackup.Attempt(dateTaken = 200L, uploaded = true),
        )
        assertEquals(90L, PhotoBackup.nextWatermark(90L, attempts))
    }

    @Test
    fun `success fail success advances only through the successful prefix`() {
        // Watermark stops at the last success BEFORE the failure (100); the
        // failed 150 AND the later-succeeded 200 are re-queried next run
        // (the 200 re-upload is idempotent via conflict=rename).
        val wm = PhotoBackup.nextWatermark(
            50L,
            listOf(
                PhotoBackup.Attempt(100L, true),
                PhotoBackup.Attempt(150L, false),
                PhotoBackup.Attempt(200L, true),
            ),
        )
        assertEquals(100L, wm)
        assertTrue(150L > wm)
    }

    @Test
    fun `all-success batch advances to the newest dateTaken`() {
        val attempts = listOf(
            PhotoBackup.Attempt(100L, true),
            PhotoBackup.Attempt(200L, true),
            PhotoBackup.Attempt(300L, true),
        )
        assertEquals(300L, PhotoBackup.nextWatermark(90L, attempts))
    }

    @Test
    fun `empty batch leaves the watermark untouched`() {
        assertEquals(7L, PhotoBackup.nextWatermark(7L, emptyList()))
    }

    @Test
    fun `all-failed batch leaves the watermark untouched`() {
        val attempts = listOf(
            PhotoBackup.Attempt(300L, false),
            PhotoBackup.Attempt(400L, false),
        )
        assertEquals(120L, PhotoBackup.nextWatermark(120L, attempts))
    }

    @Test
    fun `unsorted attempts are treated as ascending by dateTaken`() {
        // Same batch as the MediaStore would deliver it ASC; scrambled input
        // must not let a later-listed success jump over an earlier failure.
        val attempts = listOf(
            PhotoBackup.Attempt(200L, true),
            PhotoBackup.Attempt(100L, false),
        )
        assertEquals(90L, PhotoBackup.nextWatermark(90L, attempts))
    }

    @Test
    fun `watermark never moves backwards`() {
        val attempts = listOf(PhotoBackup.Attempt(5L, true))
        assertEquals(100L, PhotoBackup.nextWatermark(100L, attempts))
    }

    @Test
    fun `single success advances exactly to its dateTaken`() {
        assertEquals(
            555L,
            PhotoBackup.nextWatermark(
                500L,
                listOf(PhotoBackup.Attempt(555L, true)),
            ),
        )
    }

    @Test
    fun `DATE_TAKEN zero still counts as new via DATE_ADDED`() {
        val since = 1_700_000_000_000L
        assertTrue(
            PhotoBackup.isNewerThan(
                sinceMs = since,
                dateAddedSec = since / 1000L + 10,
                dateModifiedSec = 0L,
                dateTakenMs = 0L,
            ),
        )
    }

    @Test
    fun `DATE_ADDED zero still counts as new via DATE_TAKEN millis`() {
        val since = 1_700_000_000_000L
        assertTrue(
            PhotoBackup.isNewerThan(
                sinceMs = since,
                dateAddedSec = 0L,
                dateModifiedSec = 0L,
                dateTakenMs = since + 1,
            ),
        )
    }

    @Test
    fun `DATE_MODIFIED covers OEM cameras that zero DATE_TAKEN and DATE_ADDED`() {
        val since = 1_700_000_000_000L
        assertTrue(
            PhotoBackup.isNewerThan(
                sinceMs = since,
                dateAddedSec = 0L,
                dateModifiedSec = since / 1000L + 5,
                dateTakenMs = 0L,
            ),
        )
    }

    @Test
    fun `older timestamps on every column are not new`() {
        val since = 1_700_000_000_000L
        assertTrue(
            !PhotoBackup.isNewerThan(
                sinceMs = since,
                dateAddedSec = since / 1000L,
                dateModifiedSec = since / 1000L,
                dateTakenMs = since,
            ),
        )
    }

    @Test
    fun `timestampMs ignores DATE_TAKEN zero and prefers the newest real clock`() {
        assertEquals(5_000L, PhotoBackup.timestampMs(5, 1, 0))
        assertEquals(9_000L, PhotoBackup.timestampMs(5, 9, 100))
        assertEquals(12_000L, PhotoBackup.timestampMs(5, 9, 12_000L))
    }

    @Test
    fun `last-success is recorded for empty runs and any upload, not all-failed`() {
        assertTrue(PhotoBackup.shouldRecordLastSuccess(emptyList()))
        assertTrue(
            PhotoBackup.shouldRecordLastSuccess(
                listOf(PhotoBackup.Attempt(1L, true), PhotoBackup.Attempt(2L, false)),
            ),
        )
        assertTrue(
            !PhotoBackup.shouldRecordLastSuccess(
                listOf(PhotoBackup.Attempt(1L, false), PhotoBackup.Attempt(2L, false)),
            ),
        )
    }

    @Test
    fun `failures keep newest first and drop a URI that later uploads`() {
        val previous = listOf(
            PhotoBackup.Failure("content://old", "old.jpg", "timeout"),
            PhotoBackup.Failure("content://keep", "keep.jpg", "HTTP 500"),
        )
        val attempts = listOf(
            PhotoBackup.Attempt(100L, true, "content://old", "old.jpg"),
            PhotoBackup.Attempt(200L, false, "content://new", "new.jpg", "cannot open"),
        )
        assertEquals(
            listOf(
                PhotoBackup.Failure("content://new", "new.jpg", "cannot open"),
                PhotoBackup.Failure("content://keep", "keep.jpg", "HTTP 500"),
            ),
            PhotoBackup.rememberFailures(previous, attempts),
        )
    }

    @Test
    fun `a later failure for the same URI replaces the older message`() {
        val previous = listOf(PhotoBackup.Failure("content://a", "a.jpg", "old"))
        val attempts = listOf(
            PhotoBackup.Attempt(1L, false, "content://a", "a.jpg", "HTTP 503"),
        )
        assertEquals(
            listOf(PhotoBackup.Failure("content://a", "a.jpg", "HTTP 503")),
            PhotoBackup.rememberFailures(previous, attempts),
        )
    }

    @Test
    fun `failure list is capped at MAX_FAILURES newest first`() {
        val previous = (1..PhotoBackup.MAX_FAILURES).map {
            PhotoBackup.Failure("content://$it", "$it.jpg", "old")
        }
        val attempts = listOf(
            PhotoBackup.Attempt(9L, false, "content://n", "n.jpg", "boom"),
        )
        val next = PhotoBackup.rememberFailures(previous, attempts)
        assertEquals(PhotoBackup.MAX_FAILURES, next.size)
        assertEquals("content://n", next.first().uri)
        assertEquals("content://9", next.last().uri)
    }

    @Test
    fun `failure JSON round-trips quotes unicode and newlines`() {
        val rows = listOf(
            PhotoBackup.Failure("content://a", "naïve \"file\".jpg", "HTTP 500\nretry"),
            PhotoBackup.Failure("content://b", "b.jpg", "cannot open"),
        )
        assertEquals(rows, PhotoBackup.decodeFailures(PhotoBackup.encodeFailures(rows)))
        assertEquals(emptyList<PhotoBackup.Failure>(), PhotoBackup.decodeFailures(null))
        assertEquals(emptyList<PhotoBackup.Failure>(), PhotoBackup.decodeFailures("not-json"))
    }

    @Test
    fun `metered hint is in-flight when running on metered, else waiting for unmetered`() {
        assertEquals(
            PhotoBackup.MeteredHint.METERED_IN_FLIGHT,
            PhotoBackup.meteredHint(
                backupEnabled = true, running = true, wifiOnly = false,
                connected = true, metered = true,
            ),
        )
        assertEquals(
            PhotoBackup.MeteredHint.WAITING_UNMETERED,
            PhotoBackup.meteredHint(
                backupEnabled = true, running = false, wifiOnly = true,
                connected = true, metered = true,
            ),
        )
        assertEquals(
            PhotoBackup.MeteredHint.WAITING_UNMETERED,
            PhotoBackup.meteredHint(
                backupEnabled = true, running = false, wifiOnly = true,
                connected = false, metered = false,
            ),
        )
        assertEquals(
            PhotoBackup.MeteredHint.HIDDEN,
            PhotoBackup.meteredHint(
                backupEnabled = true, running = true, wifiOnly = true,
                connected = true, metered = false,
            ),
        )
        assertEquals(
            PhotoBackup.MeteredHint.HIDDEN,
            PhotoBackup.meteredHint(
                backupEnabled = false, running = true, wifiOnly = true,
                connected = true, metered = true,
            ),
        )
    }

    @Test
    fun `worker persists failures and settings layout shows them`() {
        val worker = sequenceOf(
            File("src/main/java/com/piercingxx/xxdrive/PhotoUploadWorker.kt"),
            File("app/src/main/java/com/piercingxx/xxdrive/PhotoUploadWorker.kt"),
        ).first { it.exists() }.readText()
        assertTrue(worker.contains("KEY_LAST_FAILURES"))
        assertTrue(worker.contains("rememberFailures"))
        val settings = sequenceOf(
            File("src/main/res/layout/activity_settings.xml"),
            File("app/src/main/res/layout/activity_settings.xml"),
        ).first { it.exists() }.readText()
        assertTrue(settings.contains("@+id/lastBackupText"))
        assertTrue(settings.contains("@+id/backupMeteredText"))
        assertTrue(settings.contains("@+id/backupErrorsText"))
    }
}
