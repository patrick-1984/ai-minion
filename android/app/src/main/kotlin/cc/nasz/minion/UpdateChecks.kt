package cc.nasz.minion

import java.io.File
import java.nio.file.Files
import java.security.MessageDigest

// Pure checks of the APK self-update, free of Android types so the JVM unit
// tests cover them. ApkUpdater feeds them what Package Manager reports.

/** The device setting: the local veto over updates the Hub requests. */
internal enum class UpdatePolicy(val key: String, val label: String) {
    AUTOMATIC("automatic", "Automatic"),
    ASK("ask", "Ask on this screen"),
    OFF("off", "Off");

    companion object {
        fun of(key: String?): UpdatePolicy = entries.firstOrNull { it.key == key } ?: AUTOMATIC
    }
}

/**
 * apk-request.json: an APK the Go Worker downloaded, verified against the
 * signed release and staged in the app's private storage. The Worker writes
 * it only at an idle moment (or for Update now) and holds new sessions, so
 * its presence is the Worker's approval of the restart. [expiresAt] (epoch
 * ms, 0 none) bounds when the app may take it.
 */
internal data class PendingApk(
    val path: String,
    val versionCode: Long,
    val sha256: String,
    val version: String,
    val tag: String,
    val id: String,
    val approved: Boolean,
    val packageName: String = "cc.nasz.minion",
    val size: Long = 0,
    val expiresAt: Long = 0,
) {
    /** One install attempt belongs to exactly one staged file. */
    val key: String get() = "$id|$versionCode|$sha256"
}

/** What getPackageArchiveInfo says about the staged APK. */
internal data class ArchiveFacts(
    val packageName: String?,
    val versionCode: Long,
    /** SHA-256 (hex) of the DER encoding of each current signer. */
    val signers: List<String>,
    val multipleSigners: Boolean,
    val targetSdk: Int,
)

internal enum class Step { OFF, BLOCKED, WAIT_APPROVAL, WAIT_IDLE, WAIT_THROTTLE, INSTALL }

internal object UpdateChecks {
    /** Upper bound of an APK we stream into a session (the universal APK is tens of MB). */
    const val MAX_APK_BYTES = 512L shl 20

    /** Our own gap between silent installs; Android's throttle is 30 s of uptime per installer/package. */
    const val SILENT_GAP_MILLIS = 60_000L

    /** Automatic tries of one staged APK before it needs the owner. */
    const val MAX_TRIES = 3

    private val SHA256 = Regex("[0-9a-f]{64}")

    fun hex(bytes: ByteArray): String = bytes.joinToString("") { "%02x".format(it.toInt() and 0xff) }

    fun sha256(bytes: ByteArray): String = hex(MessageDigest.getInstance("SHA-256").digest(bytes))

    fun normalizeDigest(value: String): String = value.trim().lowercase()

    fun validDigest(value: String): Boolean = SHA256.matches(value)

    /** Why a request cannot be used at all, or null. [stageDir] is <state>/update/apk. */
    fun pendingProblem(pending: PendingApk, stageDir: File, expectedPackage: String): String? {
        if (pending.id.isBlank()) return "the update request has no id"
        if (pending.tag.isBlank()) return "the update request has no release tag"
        if (!validDigest(pending.sha256)) return "the update request has no valid SHA-256"
        if (pending.versionCode <= 0) return "the update request has no version code"
        if (pending.packageName != expectedPackage) return "the update request is for another app (${pending.packageName})"
        return pathProblem(pending.path, stageDir)
    }

    /**
     * A request the app did not take before its expiry is left to the Worker,
     * which withdraws it; once taken (reported as installing or waiting for a
     * confirmation) it stays valid until the Worker removes it.
     */
    fun expired(expiresAt: Long, now: Long, taken: Boolean): Boolean = !taken && expiresAt > 0 && now > expiresAt

    /**
     * What `worker apk-verify` authenticated (Sigstore over the staged
     * checksums.txt, offline) must be exactly what the request names and what
     * the open file holds.
     */
    fun verifiedProblem(
        pending: PendingApk, tag: String, versionCode: Long, sha256: String, size: Long, sameFile: Boolean, fileSize: Long,
    ): String? {
        if (tag != pending.tag) return "the signed release is ${tag.ifBlank { "unknown" }}, the request names ${pending.tag}"
        if (versionCode != pending.versionCode) return "the signed release has version code $versionCode, the request ${pending.versionCode}"
        if (normalizeDigest(sha256) != pending.sha256) return "the staged APK is not the one the signed checksums list"
        if (!sameFile) return "the signed check covered another file"
        if (size != fileSize || pending.size > 0 && pending.size != fileSize) return "the staged APK has another size than the signed release"
        return null
    }

    /**
     * The staged file must be a plain .apk directly inside the update folder:
     * no symlink, no "..", no other folder of the app.
     */
    fun pathProblem(path: String, updateDir: File): String? {
        if (path.isBlank() || !path.startsWith("/")) return "the staged update path is not absolute"
        val file = File(path)
        if (!file.name.endsWith(".apk") || file.name.startsWith(".")) return "the staged update is not an .apk file"
        if (path.split('/').any { it == "." || it == ".." }) return "the staged update path is not plain"
        // Compare the folders resolved (on Android /data/user/0 is a link to
        // /data/data), and refuse a link as the file itself.
        val parent = runCatching { file.parentFile?.canonicalFile }.getOrNull() ?: return "the staged update path cannot be resolved"
        val dir = runCatching { updateDir.canonicalFile }.getOrNull() ?: return "the update folder cannot be resolved"
        if (parent != dir) return "the staged update is outside the app's update folder"
        if (Files.isSymbolicLink(file.toPath())) return "the staged update is a link"
        return null
    }

    /**
     * The defense-in-depth check of the APK against the app: same package,
     * the version the Worker verified, strictly newer than the running app,
     * exactly one signer and that signer is the pinned release certificate.
     */
    fun archiveProblem(
        facts: ArchiveFacts?,
        pending: PendingApk,
        expectedPackage: String,
        installedCode: Long,
        pinned: String,
    ): String? {
        if (facts == null) return "Android cannot read the staged APK"
        if (facts.packageName != expectedPackage) return "the APK is for another app (${facts.packageName ?: "unknown"})"
        if (facts.versionCode != pending.versionCode) {
            return "the APK has version code ${facts.versionCode}, the Worker expected ${pending.versionCode}"
        }
        if (facts.versionCode <= installedCode) {
            return "the APK (version code ${facts.versionCode}) is not newer than this app ($installedCode)"
        }
        val pin = normalizeDigest(pinned)
        if (!validDigest(pin)) return "this app has no pinned signing certificate"
        if (facts.multipleSigners || facts.signers.size != 1) return "the APK must have exactly one signer"
        if (normalizeDigest(facts.signers[0]) != pin) return "the APK is not signed with the Minion release key"
        return null
    }

    /** Whether this app itself carries the pinned certificate (a debug build does not). */
    fun selfSigned(signers: List<String>, pinned: String): Boolean {
        val pin = normalizeDigest(pinned)
        return validDigest(pin) && signers.map(::normalizeDigest) == listOf(pin)
    }

    /**
     * The lowest targetSdk of the incoming APK that Android accepts for an
     * update without user action (setRequireUserAction javadoc and AOSP:
     * 29 on Android 12, 30 on 13, 31 on 14, 33 from 15). 0: never silent.
     */
    fun silentTargetSdk(sdkInt: Int): Int = when {
        sdkInt < 31 -> 0
        sdkInt <= 32 -> 29
        sdkInt == 33 -> 30
        sdkInt == 34 -> 31
        else -> 33
    }

    /**
     * How long to wait before the next silent commit, from the last one.
     * Uptime restarts at boot, so a record of another boot does not count.
     */
    fun throttleWait(lastUptime: Long, lastBoot: Int, nowUptime: Long, nowBoot: Int, gap: Long = SILENT_GAP_MILLIS): Long {
        if (lastUptime <= 0 || lastBoot != nowBoot || lastUptime > nowUptime) return 0
        return maxOf(0, lastUptime + gap - nowUptime)
    }

    /**
     * What to do with a staged, valid APK. [liveSessions] is the Worker's
     * session count (-1 unknown). [userRequested]: the owner pressed Install
     * update on this screen, which is the approval for everything but Off.
     */
    fun decide(
        policy: UpdatePolicy,
        canInstall: Boolean,
        selfSigned: Boolean,
        sdkInt: Int,
        liveSessions: Int,
        approved: Boolean,
        userRequested: Boolean,
        throttleWait: Long,
    ): Pair<Step, String> = when {
        policy == UpdatePolicy.OFF -> Step.OFF to "updates are turned off on this device"
        !selfSigned -> Step.BLOCKED to "this copy of the app is not signed with the Minion release key"
        !canInstall -> Step.BLOCKED to "Allow Minion to install updates (Install unknown apps) on this device"
        userRequested -> Step.INSTALL to ""
        policy == UpdatePolicy.ASK -> Step.WAIT_APPROVAL to "waiting for Install update on this device"
        sdkInt < 31 -> Step.WAIT_APPROVAL to "Android ${androidName(sdkInt)} needs a confirmation on this device"
        liveSessions != 0 && !approved ->
            Step.WAIT_IDLE to if (liveSessions < 0) "waiting for the Worker's session list" else
                "waiting until no terminal session runs ($liveSessions running), or approve the restart"
        throttleWait > 0 -> Step.WAIT_THROTTLE to "waiting ${(throttleWait + 999) / 1000} s before the next silent update"
        else -> Step.INSTALL to ""
    }

    private fun androidName(sdkInt: Int) = when (sdkInt) {
        29 -> "10"
        30 -> "11"
        else -> "API $sdkInt"
    }
}
