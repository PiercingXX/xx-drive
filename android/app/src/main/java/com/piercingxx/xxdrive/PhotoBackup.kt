package com.piercingxx.xxdrive

/** Pure watermark math for camera auto-backup; JVM-unit-testable. */
object PhotoBackup {

    /** One batch entry: when the photo was taken and whether its upload succeeded. */
    data class Attempt(val dateTaken: Long, val uploaded: Boolean)

    /**
     * Whether a MediaStore row is newer than [sinceMs].
     *
     * DATE_ADDED and DATE_MODIFIED are epoch **seconds**; DATE_TAKEN is epoch
     * **milliseconds** (0 means the OEM left it unset — Graphene/Pixel is
     * fine, some OEM cameras write 0 forever). Any one of the three beating
     * the watermark is enough, so DATE_TAKEN=0 still backs up via DATE_ADDED
     * or DATE_MODIFIED.
     */
    fun isNewerThan(
        sinceMs: Long,
        dateAddedSec: Long,
        dateModifiedSec: Long,
        dateTakenMs: Long,
    ): Boolean {
        val sinceSec = sinceMs / 1000L
        return dateAddedSec > sinceSec ||
            dateModifiedSec > sinceSec ||
            dateTakenMs > sinceMs
    }

    /**
     * Best epoch-millis timestamp for watermark and `Camera Uploads/<date>/`.
     * DATE_TAKEN=0 is treated as unset, not as 1970.
     */
    fun timestampMs(dateAddedSec: Long, dateModifiedSec: Long, dateTakenMs: Long): Long {
        val taken = if (dateTakenMs > 0L) dateTakenMs else 0L
        val added = if (dateAddedSec > 0L) dateAddedSec * 1000L else 0L
        val modified = if (dateModifiedSec > 0L) dateModifiedSec * 1000L else 0L
        return maxOf(taken, added, modified)
    }

    /** Record a last-success wall clock when the run was empty or uploaded at least one. */
    fun shouldRecordLastSuccess(attempts: List<Attempt>): Boolean =
        attempts.isEmpty() || attempts.any { it.uploaded }

    /**
     * The next watermark is the dateTaken at the end of the longest ALL-successful
     * PREFIX of the batch (attempts sorted ascending by dateTaken): advancing past
     * a failed photo would permanently skip it, so the watermark stops at the first
     * failure even if newer files in the same batch succeeded — those get re-queried
     * and re-uploaded next run (conflict=rename makes the retry idempotent). If the
     * oldest attempt fails, nothing advances. Never moves backwards.
     */
    fun nextWatermark(currentTs: Long, attempts: List<Attempt>): Long {
        var next = currentTs
        for (attempt in attempts.sortedBy { it.dateTaken }) {
            if (!attempt.uploaded) break
            if (attempt.dateTaken > next) next = attempt.dateTaken
        }
        return next
    }
}
