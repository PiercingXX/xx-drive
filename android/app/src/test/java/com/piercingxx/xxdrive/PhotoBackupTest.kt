package com.piercingxx.xxdrive

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

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
}
