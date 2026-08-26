package com.piercingxx.xxdrive

/** Pure watermark math for camera auto-backup; JVM-unit-testable. */
object PhotoBackup {

    /** One batch entry: when the photo was taken and whether its upload succeeded. */
    data class Attempt(val dateTaken: Long, val uploaded: Boolean)

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
