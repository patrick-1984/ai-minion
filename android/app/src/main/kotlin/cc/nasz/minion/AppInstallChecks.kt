package cc.nasz.minion

import java.io.File
import java.io.FileInputStream
import java.io.FileOutputStream
import java.nio.file.Files
import java.nio.file.LinkOption
import java.security.MessageDigest

// Pure checks of installing another app from the Minion files folder
// (app_install, app.install.v1), free of Android types so the JVM unit tests
// cover them. AppInstaller feeds them what Package Manager reports.

/**
 * app-install-pending.json: an APK in the Minion files folder that the Go
 * Worker checked (confinement, size, SHA-256) and asks the app to install.
 * [expiresAt] (epoch ms, 0 none) bounds when the app may take it.
 */
internal data class PendingAppInstall(
    val id: String,
    val path: String,
    val relPath: String,
    val sha256: String,
    val size: Long,
    val allowNew: Boolean,
    val expiresAt: Long = 0,
)

/** What getPackageArchiveInfo says about the APK to install. */
internal data class AppArchiveFacts(
    val packageName: String?,
    val versionCode: Long,
    val versionName: String,
    val label: String,
    /** SHA-256 (hex) of the DER encoding of each current signer. */
    val signers: List<String>,
)

/** The installed package of the same name, or null when it is not installed. */
internal data class InstalledApp(
    val versionCode: Long,
    val signers: List<String>,
)

internal object AppInstallChecks {
    /** Upper bound of an APK the app streams into an install session. */
    const val MAX_APK_BYTES = 512L shl 20

    /** Why the request cannot be used at all, or null. [filesDir] is the Minion files folder. */
    fun requestProblem(request: PendingAppInstall, filesDir: File): String? {
        if (request.id.isBlank()) return "the install request has no id"
        if (!UpdateChecks.validDigest(request.sha256)) return "the install request has no valid SHA-256"
        if (request.size <= 0 || request.size > MAX_APK_BYTES) return "the APK has an invalid size (${request.size} bytes)"
        return pathProblem(request.path, request.size, filesDir)
    }

    /**
     * The APK must be a plain, regular .apk file inside the Minion files
     * folder: absolute, no "." or "..", no link (neither the file nor a
     * folder on the way), and of the size the Worker measured.
     */
    fun pathProblem(path: String, size: Long, filesDir: File): String? {
        if (path.isBlank() || !path.startsWith("/")) return "the APK path is not absolute"
        if (path.split('/').drop(1).any { it.isEmpty() || it == "." || it == ".." }) return "the APK path is not plain"
        val file = File(path)
        if (!file.name.lowercase().endsWith(".apk") || file.name.startsWith(".")) return "the file is not an .apk file"
        val dir = runCatching { filesDir.canonicalFile }.getOrNull() ?: return "the Minion files folder cannot be resolved"
        // The path as written must already be inside the folder (the folder's
        // own path may contain a system link such as /data/user/0).
        val base = filesDir.absolutePath.trimEnd('/')
        val canonicalBase = dir.path.trimEnd('/')
        val relative = when {
            path.startsWith("$base/") -> path.removePrefix("$base/")
            path.startsWith("$canonicalBase/") -> path.removePrefix("$canonicalBase/")
            else -> return "the APK is outside the Minion files folder"
        }
        // No link anywhere below the folder.
        var current = dir
        for (part in relative.split('/')) {
            current = File(current, part)
            if (Files.isSymbolicLink(current.toPath())) return "the APK path contains a link"
        }
        val canonical = runCatching { file.canonicalFile }.getOrNull() ?: return "the APK path cannot be resolved"
        if (!canonical.path.startsWith("$canonicalBase/")) return "the APK is outside the Minion files folder"
        if (!Files.isRegularFile(current.toPath(), LinkOption.NOFOLLOW_LINKS)) return "the APK is not a regular file"
        if (current.length() != size) return CHANGED
        return null
    }

    /**
     * Whether the APK may be installed, or why not. [own] is this app's
     * package; [installed] the installed app of the same package, or null.
     */
    fun problem(facts: AppArchiveFacts?, own: String, installed: InstalledApp?, allowNew: Boolean): String? {
        if (facts == null || facts.packageName.isNullOrBlank()) return "Android cannot read the APK"
        if (facts.packageName == own) return "Minion updates itself with its own update path"
        if (facts.signers.isEmpty()) return "the APK has no signer"
        if (installed == null) {
            return if (allowNew) null else "${facts.packageName} is not installed on this device; pass --allow-new to install a new app"
        }
        if (normalized(facts.signers) != normalized(installed.signers)) {
            return "the APK is signed with another key than the installed app"
        }
        if (facts.versionCode < installed.versionCode) {
            return "refusing a downgrade from version code ${installed.versionCode} to ${facts.versionCode}"
        }
        if (facts.versionCode == installed.versionCode && !allowNew) {
            return "already installed at this version; pass --allow-new to reinstall"
        }
        return null
    }

    /**
     * Copies the APK [source] into [staged], the app's private copy, from one
     * open of the file and hashing exactly the bytes it copies. Package
     * Manager then reads and the install session then gets that copy only,
     * so the checks, the audited facts and the install describe the same
     * bytes, which are the bytes the Worker hashed: a file swapped in the
     * Minion files folder meanwhile cannot be checked as one APK and installed
     * as another. Returns why the copy cannot be used (and removes it), or
     * null when [staged] holds exactly [size] bytes with SHA-256 [sha256].
     * I/O errors are thrown (and the copy removed).
     */
    fun stage(source: File, size: Long, sha256: String, staged: File): String? {
        val problem = try {
            copyHashed(source, size, UpdateChecks.normalizeDigest(sha256), staged)
        } catch (e: Exception) {
            staged.delete()
            throw e
        }
        if (problem != null) staged.delete()
        return problem
    }

    private fun copyHashed(source: File, size: Long, sha256: String, staged: File): String? {
        staged.delete()
        FileInputStream(source).use { input ->
            // The size and every byte come from this one open file.
            val length = input.channel.size()
            if (length != size || length <= 0 || length > MAX_APK_BYTES) return CHANGED
            val digest = MessageDigest.getInstance("SHA-256")
            FileOutputStream(staged).use { out ->
                val buffer = ByteArray(64 * 1024)
                var total = 0L
                while (true) {
                    val n = input.read(buffer)
                    if (n < 0) break
                    total += n
                    if (total > length) return CHANGED_WHILE_READ
                    digest.update(buffer, 0, n)
                    out.write(buffer, 0, n)
                }
                if (total != length) return CHANGED_WHILE_READ
                out.fd.sync()
            }
            if (UpdateChecks.hex(digest.digest()) != sha256) return DIGEST_MISMATCH
        }
        return null
    }

    const val CHANGED = "the APK changed after the Worker checked it"
    const val CHANGED_WHILE_READ = "the APK changed while it was read"
    const val DIGEST_MISMATCH = "the APK does not match the SHA-256 the Worker computed"

    /** The signer digests joined, as the status file reports them. */
    fun signerText(signers: List<String>): String = normalized(signers).joinToString(",")

    private fun normalized(signers: List<String>) = signers.map(UpdateChecks::normalizeDigest).sorted()
}
