package com.piercingxx.xxdrive

/** Pure watermark, failure-list, and metered-hint math for camera auto-backup. */
object PhotoBackup {

    /** How many per-photo failures Settings keeps (newest first). */
    const val MAX_FAILURES = 10

    /** One batch entry: when the photo was taken and whether its upload succeeded. */
    data class Attempt(
        val dateTaken: Long,
        val uploaded: Boolean,
        val uri: String = "",
        val name: String = "",
        val error: String = "",
    )

    /** One persisted failure row shown on the backup/settings screen. */
    data class Failure(val uri: String, val name: String, val message: String)

    /** Metered copy while backup is in flight, or waiting for unmetered. */
    enum class MeteredHint { HIDDEN, METERED_IN_FLIGHT, WAITING_UNMETERED }

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

    /**
     * Keep the last [limit] failures, newest first. A URI that uploaded this
     * run is dropped; a URI that failed again replaces its older row.
     */
    fun rememberFailures(
        previous: List<Failure>,
        attempts: List<Attempt>,
        limit: Int = MAX_FAILURES,
    ): List<Failure> {
        val succeeded = attempts.mapNotNull { if (it.uploaded) it.uri.takeIf(String::isNotEmpty) else null }.toSet()
        val incoming = attempts
            .filter { !it.uploaded && it.uri.isNotEmpty() }
            .map { Failure(it.uri, it.name, it.error.ifBlank { "upload failed" }) }
            .asReversed()
        val incomingUris = incoming.map { it.uri }.toSet()
        val kept = previous.filterNot { it.uri in succeeded || it.uri in incomingUris }
        return (incoming + kept).take(limit)
    }

    fun encodeFailures(failures: List<Failure>): String {
        val sb = StringBuilder()
        sb.append('[')
        failures.forEachIndexed { i, f ->
            if (i > 0) sb.append(',')
            sb.append("{\"uri\":").append(jsonString(f.uri))
                .append(",\"name\":").append(jsonString(f.name))
                .append(",\"message\":").append(jsonString(f.message))
                .append('}')
        }
        sb.append(']')
        return sb.toString()
    }

    fun decodeFailures(raw: String?): List<Failure> {
        if (raw.isNullOrBlank()) return emptyList()
        return try {
            parseFailureArray(raw)
        } catch (_: Exception) {
            emptyList()
        }
    }

    /**
     * [running] is WorkManager RUNNING. Periodic work sits ENQUEUED while idle,
     * so wifi-only + metered/offline is the wait — not ENQUEUED itself.
     */
    fun meteredHint(
        backupEnabled: Boolean,
        running: Boolean,
        wifiOnly: Boolean,
        connected: Boolean,
        metered: Boolean,
    ): MeteredHint {
        if (!backupEnabled) return MeteredHint.HIDDEN
        if (running && metered) return MeteredHint.METERED_IN_FLIGHT
        if (!running && wifiOnly && (!connected || metered)) return MeteredHint.WAITING_UNMETERED
        return MeteredHint.HIDDEN
    }

    private fun jsonString(value: String): String {
        val sb = StringBuilder(value.length + 2)
        sb.append('"')
        for (c in value) {
            when (c) {
                '\\' -> sb.append("\\\\")
                '"' -> sb.append("\\\"")
                '\n' -> sb.append("\\n")
                '\r' -> sb.append("\\r")
                '\t' -> sb.append("\\t")
                else -> if (c.code < 0x20) {
                    sb.append("\\u").append(c.code.toString(16).padStart(4, '0'))
                } else {
                    sb.append(c)
                }
            }
        }
        sb.append('"')
        return sb.toString()
    }

    private fun parseFailureArray(raw: String): List<Failure> {
        val r = JsonReader(raw)
        r.expect('[')
        val out = mutableListOf<Failure>()
        if (r.peek() == ']') {
            r.expect(']')
            return out
        }
        while (true) {
            out.add(parseFailureObject(r))
            when (r.peek()) {
                ',' -> r.expect(',')
                ']' -> {
                    r.expect(']')
                    return out
                }
                else -> error("expected comma or end of array")
            }
        }
    }

    private fun parseFailureObject(r: JsonReader): Failure {
        r.expect('{')
        var uri = ""
        var name = ""
        var message = ""
        if (r.peek() == '}') {
            r.expect('}')
            return Failure(uri, name, message)
        }
        while (true) {
            val key = r.readString()
            r.expect(':')
            val value = r.readString()
            when (key) {
                "uri" -> uri = value
                "name" -> name = value
                "message" -> message = value
            }
            when (r.peek()) {
                ',' -> r.expect(',')
                '}' -> {
                    r.expect('}')
                    return Failure(uri, name, message)
                }
                else -> error("expected comma or end of object")
            }
        }
    }

    private class JsonReader(private val s: String) {
        var i = 0

        fun peek(): Char {
            skipWs()
            check(i < s.length) { "unexpected end" }
            return s[i]
        }

        fun expect(c: Char) {
            skipWs()
            check(i < s.length && s[i] == c) { "expected $c" }
            i++
        }

        fun readString(): String {
            skipWs()
            check(i < s.length && s[i] == '"') { "expected string" }
            i++
            val sb = StringBuilder()
            while (i < s.length) {
                val c = s[i++]
                when (c) {
                    '"' -> return sb.toString()
                    '\\' -> {
                        check(i < s.length) { "unterminated escape" }
                        when (val e = s[i++]) {
                            '"', '\\', '/' -> sb.append(e)
                            'b' -> sb.append('\b')
                            'f' -> sb.append('\u000C')
                            'n' -> sb.append('\n')
                            'r' -> sb.append('\r')
                            't' -> sb.append('\t')
                            'u' -> {
                                check(i + 4 <= s.length) { "bad unicode escape" }
                                val hex = s.substring(i, i + 4)
                                sb.append(hex.toInt(16).toChar())
                                i += 4
                            }
                            else -> error("bad escape")
                        }
                    }
                    else -> sb.append(c)
                }
            }
            error("unterminated string")
        }

        private fun skipWs() {
            while (i < s.length && s[i].isWhitespace()) i++
        }
    }
}
