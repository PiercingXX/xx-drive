package com.piercingxx.xxdrive

import java.net.URLDecoder

/**
 * Pure filename resolution for DownloadManager downloads: Content-Disposition
 * first (RFC 6266 filename= / RFC 5987 filename*=), then the URL path, then a
 * fixed fallback — always uniquified so repeated downloads of the same file
 * cannot collide while keeping the original name + extension.
 */
object DownloadNames {
    const val FALLBACK_BASE = "download"
    const val UNIQUE_PREFIX = "xxdrive"

    /** Full pipeline: headers + url + timestamp -> final stored filename. */
    fun displayName(contentDisposition: String?, url: String?, timestampMs: Long): String =
        unique(pickName(contentDisposition, url), timestampMs)

    fun pickName(contentDisposition: String?, url: String?): String =
        sanitize(
            fromContentDisposition(contentDisposition)
                ?: fromUrl(url)
                ?: FALLBACK_BASE,
        )

    /** RFC 6266 header value; filename* (RFC 5987, percent-encoded) wins over plain filename=. */
    fun fromContentDisposition(cd: String?): String? {
        if (cd.isNullOrBlank()) return null
        val star = Regex("filename\\*\\s*=\\s*([^;]+)", RegexOption.IGNORE_CASE).find(cd)
        if (star != null) {
            decodeRfc5987(star.groupValues[1].trim())?.let { if (it.isNotBlank()) return it }
        }
        val m = Regex("filename\\s*=\\s*(\"([^\"]*)\"|[^;\\s][^;]*)", RegexOption.IGNORE_CASE).find(cd)
            ?: return null
        val raw = m.groupValues[2].ifEmpty { m.groupValues[1] }.trim().trim('"').trim()
        return raw.ifBlank { null }
    }

    private fun decodeRfc5987(value: String): String? = try {
        val encoded = if (value.contains("''")) value.substringAfter("''") else value
        URLDecoder.decode(encoded, "UTF-8")
    } catch (_: Exception) {
        null
    }

    /** Last non-empty path segment of the URL, query/fragment stripped, percent-decoded. */
    fun fromUrl(url: String?): String? {
        if (url.isNullOrBlank()) return null
        val schemeEnd = url.indexOf("://")
        val rest = url.substring(if (schemeEnd >= 0) schemeEnd + 3 else 0)
        val slash = rest.indexOf('/')
        if (slash < 0) return null // authority only, no path
        val path = rest.substring(slash + 1).substringBefore('?').substringBefore('#')
        val segment = path.trimEnd('/').substringAfterLast('/')
        if (segment.isBlank()) return null
        return try {
            URLDecoder.decode(segment, "UTF-8")
        } catch (_: Exception) {
            segment
        }
    }

    /** Strip filesystem-hostile characters; empty results fall back to [FALLBACK_BASE]. */
    fun sanitize(name: String): String {
        val cleaned = name
            .replace(Regex("[\\\\/]"), "_")
            .replace(Regex("[\"*?:<>|]"), "_")
            .replace(Regex("\\p{Cntrl}"), "")
            .trim(' ', '.')
        return cleaned.ifBlank { FALLBACK_BASE }
    }

    /** Prefix + timestamp guarantees distinct destinations across repeat downloads. */
    fun unique(base: String, timestampMs: Long): String =
        "$UNIQUE_PREFIX-$timestampMs-${sanitize(base)}"
}
