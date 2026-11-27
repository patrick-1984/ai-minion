package cc.nasz.minion

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File
import java.nio.file.Files

class UpdateChecksTest {
    @get:Rule val temp = TemporaryFolder()

    private val pin = "569c16bc870b076f5d5257883930163e9e3c6f95d5027c1214f813dbc695121f"
    private val other = "0".repeat(64)
    private val digest = "a".repeat(64)

    private fun pending(path: String = "/x/update/minion-android-0.24.0.apk", code: Long = 2_400_999, sha: String = digest) =
        PendingApk(path, code, sha, "0.24.0", "v0.24.0", "u1", approved = false)

    private fun facts(
        pkg: String? = "cc.nasz.minion", code: Long = 2_400_999, signers: List<String> = listOf(pin), multiple: Boolean = false,
    ) = ArchiveFacts(pkg, code, signers, multiple, 35)

    @Test fun sha256IsLowercaseHex() {
        assertEquals("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", UpdateChecks.sha256("abc".toByteArray()))
    }

    @Test fun policyDefaultsToAutomatic() {
        assertEquals(UpdatePolicy.AUTOMATIC, UpdatePolicy.of(null))
        assertEquals(UpdatePolicy.AUTOMATIC, UpdatePolicy.of("nonsense"))
        assertEquals(UpdatePolicy.ASK, UpdatePolicy.of("ask"))
        assertEquals(UpdatePolicy.OFF, UpdatePolicy.of("off"))
    }

    @Test fun pathMustBeAnApkDirectlyInTheUpdateFolder() {
        val dir = temp.newFolder("state", "update")
        val apk = File(dir, "minion-android-0.24.0.apk").apply { writeText("x") }
        assertNull(UpdateChecks.pathProblem(apk.path, dir))
        assertNotNull(UpdateChecks.pathProblem("minion.apk", dir))
        assertNotNull(UpdateChecks.pathProblem("", dir))
        assertNotNull(UpdateChecks.pathProblem(File(dir, "minion.zip").path, dir))
        assertNotNull(UpdateChecks.pathProblem(File(dir, ".hidden.apk").path, dir))
        assertNotNull(UpdateChecks.pathProblem("${dir.path}/../update/minion.apk", dir))
        assertNotNull(UpdateChecks.pathProblem(File(temp.root, "outside.apk").path, dir))
        val sub = File(dir, "sub").apply { mkdirs() }
        assertNotNull(UpdateChecks.pathProblem(File(sub, "minion.apk").path, dir))
    }

    @Test fun symlinkedApkIsRefused() {
        val dir = temp.newFolder("update")
        val real = temp.newFile("elsewhere.apk")
        val link = File(dir, "minion.apk")
        Files.createSymbolicLink(link.toPath(), real.toPath())
        assertNotNull(UpdateChecks.pathProblem(link.path, dir))
    }

    // On Android /data/user/0 is a link to /data/data: the Worker and the app
    // may name the same folder either way.
    @Test fun linkedParentFolderIsTheSameFolder() {
        val real = temp.newFolder("data", "cc.nasz.minion", "state", "update")
        val user0 = File(temp.root, "user0")
        Files.createSymbolicLink(user0.toPath(), File(temp.root, "data").toPath())
        val viaLink = File(user0, "cc.nasz.minion/state/update")
        File(real, "m.apk").writeText("x")
        assertNull(UpdateChecks.pathProblem(File(real, "m.apk").path, viaLink))
        assertNull(UpdateChecks.pathProblem(File(viaLink, "m.apk").path, real))
    }

    @Test fun requestNeedsIdTagDigestCodePackageAndPath() {
        val dir = temp.newFolder("update", "apk")
        val ok = pending(path = File(dir, "m.apk").path)
        val app = "cc.nasz.minion"
        assertNull(UpdateChecks.pendingProblem(ok, dir, app))
        assertNotNull(UpdateChecks.pendingProblem(ok.copy(sha256 = "abc"), dir, app))
        assertNotNull(UpdateChecks.pendingProblem(ok.copy(sha256 = digest.uppercase()), dir, app))
        assertNotNull(UpdateChecks.pendingProblem(ok.copy(versionCode = 0), dir, app))
        assertNotNull(UpdateChecks.pendingProblem(ok.copy(id = " "), dir, app))
        assertNotNull(UpdateChecks.pendingProblem(ok.copy(tag = ""), dir, app))
        assertNotNull(UpdateChecks.pendingProblem(ok.copy(packageName = "com.other"), dir, app))
        assertNotNull(UpdateChecks.pendingProblem(ok.copy(path = File(dir.parentFile, "m.apk").path), dir, app))
        assertEquals("u1|2400999|$digest", ok.key)
    }

    @Test fun untakenRequestExpires() {
        assertTrue(!UpdateChecks.expired(0, 5_000, taken = false))
        assertTrue(!UpdateChecks.expired(10_000, 5_000, taken = false))
        assertTrue(UpdateChecks.expired(10_000, 10_001, taken = false))
        // Unreadable expiry is parsed as 1 ms after the epoch.
        assertTrue(UpdateChecks.expired(1, 10_001, taken = false))
        // Taken (installing or waiting for a person): the Worker decides.
        assertTrue(!UpdateChecks.expired(10_000, 99_999, taken = true))
    }

    @Test fun verifiedReleaseMustMatchRequestAndFile() {
        val p = pending().copy(size = 1234)
        assertNull(UpdateChecks.verifiedProblem(p, "v0.24.0", 2_400_999, digest.uppercase(), 1234, true, 1234))
        assertTrue(UpdateChecks.verifiedProblem(p, "v0.23.0", 2_400_999, digest, 1234, true, 1234)!!.contains("v0.23.0"))
        assertTrue(UpdateChecks.verifiedProblem(p, "v0.24.0", 2_300_999, digest, 1234, true, 1234)!!.contains("2400999"))
        assertTrue(UpdateChecks.verifiedProblem(p, "v0.24.0", 2_400_999, other, 1234, true, 1234)!!.contains("signed checksums"))
        assertTrue(UpdateChecks.verifiedProblem(p, "v0.24.0", 2_400_999, digest, 1234, false, 1234)!!.contains("another file"))
        assertTrue(UpdateChecks.verifiedProblem(p, "v0.24.0", 2_400_999, digest, 1234, true, 1235)!!.contains("size"))
        assertTrue(UpdateChecks.verifiedProblem(p.copy(size = 9), "v0.24.0", 2_400_999, digest, 1234, true, 1234)!!.contains("size"))
        // A request without a size relies on the signed size.
        assertNull(UpdateChecks.verifiedProblem(p.copy(size = 0), "v0.24.0", 2_400_999, digest, 1234, true, 1234))
    }

    @Test fun archiveMatchingEverythingPasses() {
        assertNull(UpdateChecks.archiveProblem(facts(), pending(), "cc.nasz.minion", 2_300_999, pin))
        assertNull(UpdateChecks.archiveProblem(facts(signers = listOf(pin.uppercase())), pending(), "cc.nasz.minion", 2_300_999, " $pin\n"))
    }

    @Test fun archiveRefusals() {
        val p = pending()
        val installed = 2_300_999L
        assertTrue(UpdateChecks.archiveProblem(null, p, "cc.nasz.minion", installed, pin)!!.contains("cannot read"))
        assertTrue(UpdateChecks.archiveProblem(facts(pkg = "com.evil"), p, "cc.nasz.minion", installed, pin)!!.contains("another app"))
        assertTrue(UpdateChecks.archiveProblem(facts(pkg = null), p, "cc.nasz.minion", installed, pin)!!.contains("another app"))
        assertTrue(UpdateChecks.archiveProblem(facts(code = 2_400_001), p, "cc.nasz.minion", installed, pin)!!.contains("expected"))
        // Same version and downgrade.
        assertTrue(UpdateChecks.archiveProblem(facts(), p, "cc.nasz.minion", 2_400_999, pin)!!.contains("not newer"))
        assertTrue(UpdateChecks.archiveProblem(facts(), p, "cc.nasz.minion", 2_500_999, pin)!!.contains("not newer"))
        assertTrue(UpdateChecks.archiveProblem(facts(signers = listOf(other)), p, "cc.nasz.minion", installed, pin)!!.contains("release key"))
        assertTrue(UpdateChecks.archiveProblem(facts(signers = emptyList()), p, "cc.nasz.minion", installed, pin)!!.contains("exactly one"))
        assertTrue(UpdateChecks.archiveProblem(facts(signers = listOf(pin, other)), p, "cc.nasz.minion", installed, pin)!!.contains("exactly one"))
        assertTrue(UpdateChecks.archiveProblem(facts(multiple = true), p, "cc.nasz.minion", installed, pin)!!.contains("exactly one"))
        assertTrue(UpdateChecks.archiveProblem(facts(), p, "cc.nasz.minion", installed, "")!!.contains("pinned"))
    }

    @Test fun selfSignedNeedsExactlyThePin() {
        assertTrue(UpdateChecks.selfSigned(listOf(pin), pin))
        assertTrue(!UpdateChecks.selfSigned(listOf(other), pin))
        assertTrue(!UpdateChecks.selfSigned(listOf(pin, other), pin))
        assertTrue(!UpdateChecks.selfSigned(emptyList(), pin))
        assertTrue(!UpdateChecks.selfSigned(listOf(""), ""))
    }

    @Test fun silentTargetSdkFollowsAndroid() {
        assertEquals(0, UpdateChecks.silentTargetSdk(29))
        assertEquals(0, UpdateChecks.silentTargetSdk(30))
        assertEquals(29, UpdateChecks.silentTargetSdk(31))
        assertEquals(29, UpdateChecks.silentTargetSdk(32))
        assertEquals(30, UpdateChecks.silentTargetSdk(33))
        assertEquals(31, UpdateChecks.silentTargetSdk(34))
        assertEquals(33, UpdateChecks.silentTargetSdk(35))
        assertEquals(33, UpdateChecks.silentTargetSdk(36))
    }

    @Test fun throttle() {
        assertEquals(0, UpdateChecks.throttleWait(0, -1, 100_000, 7))
        assertEquals(40_000, UpdateChecks.throttleWait(100_000, 7, 120_000, 7))
        assertEquals(0, UpdateChecks.throttleWait(100_000, 7, 160_000, 7))
        assertEquals(0, UpdateChecks.throttleWait(100_000, 7, 200_000, 7))
        // Another boot, or uptime that went back (a reboot without a boot count).
        assertEquals(0, UpdateChecks.throttleWait(100_000, 6, 120_000, 7))
        assertEquals(0, UpdateChecks.throttleWait(100_000, -1, 50_000, -1))
    }

    private fun decide(
        policy: UpdatePolicy = UpdatePolicy.AUTOMATIC, canInstall: Boolean = true, selfSigned: Boolean = true, sdk: Int = 34,
        sessions: Int = 0, approved: Boolean = false, user: Boolean = false, wait: Long = 0,
    ) = UpdateChecks.decide(policy, canInstall, selfSigned, sdk, sessions, approved, user, wait).first

    @Test fun decisions() {
        assertEquals(Step.INSTALL, decide())
        // Off is the local veto, also over the button.
        assertEquals(Step.OFF, decide(policy = UpdatePolicy.OFF, user = true))
        assertEquals(Step.BLOCKED, decide(selfSigned = false, user = true))
        assertEquals(Step.BLOCKED, decide(canInstall = false, user = true))
        assertEquals(Step.WAIT_APPROVAL, decide(policy = UpdatePolicy.ASK))
        assertEquals(Step.INSTALL, decide(policy = UpdatePolicy.ASK, user = true))
        assertEquals(Step.WAIT_APPROVAL, decide(sdk = 30))
        assertEquals(Step.WAIT_APPROVAL, decide(sdk = 29))
        assertEquals(Step.INSTALL, decide(sdk = 30, user = true))
        assertEquals(Step.WAIT_IDLE, decide(sessions = 2))
        assertEquals(Step.WAIT_IDLE, decide(sessions = -1))
        assertEquals(Step.INSTALL, decide(sessions = 2, approved = true))
        assertEquals(Step.INSTALL, decide(sessions = 2, user = true))
        assertEquals(Step.WAIT_THROTTLE, decide(wait = 5_000))
        assertEquals(Step.INSTALL, decide(wait = 5_000, user = true))
    }
}
