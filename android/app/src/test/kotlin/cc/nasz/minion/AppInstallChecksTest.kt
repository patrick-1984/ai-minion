package cc.nasz.minion

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File
import java.nio.file.Files

class AppInstallChecksTest {
    @get:Rule val temp = TemporaryFolder()

    private val key = "1".repeat(64)
    private val otherKey = "2".repeat(64)
    private val digest = "a".repeat(64)
    private val own = "cc.nasz.minion"

    private fun folder(): File = temp.newFolder("Minion files")

    private fun apk(dir: File, name: String = "app.apk", bytes: Int = 10): File =
        File(dir, name).apply { parentFile!!.mkdirs(); writeBytes(ByteArray(bytes)) }

    private fun request(path: String, size: Long = 10, sha: String = digest, id: String = "r1", allowNew: Boolean = false) =
        PendingAppInstall(id, path, File(path).name, sha, size, allowNew)

    private fun facts(pkg: String? = "com.example.app", code: Long = 5, signers: List<String> = listOf(key)) =
        AppArchiveFacts(pkg, code, "1.0", "Example", signers)

    @Test fun plainApkInsideTheFolderIsAccepted() {
        val dir = folder()
        val file = apk(File(dir, "sub"))
        assertNull(AppInstallChecks.requestProblem(request(file.path), dir))
    }

    @Test fun requestFieldsAreChecked() {
        val dir = folder()
        val file = apk(dir)
        assertTrue(AppInstallChecks.requestProblem(request(file.path, id = ""), dir)!!.contains("no id"))
        assertTrue(AppInstallChecks.requestProblem(request(file.path, sha = "xyz"), dir)!!.contains("SHA-256"))
        assertTrue(AppInstallChecks.requestProblem(request(file.path, size = 0), dir)!!.contains("invalid size"))
        assertTrue(AppInstallChecks.requestProblem(request(file.path, size = AppInstallChecks.MAX_APK_BYTES + 1), dir)!!
            .contains("invalid size"))
        assertTrue(AppInstallChecks.requestProblem(request(file.path, size = 11), dir)!!.contains("changed"))
    }

    @Test fun pathOutsideTheFolderIsRefused() {
        val dir = folder()
        val outside = apk(temp.newFolder("elsewhere"))
        assertEquals("the APK is outside the Minion files folder", AppInstallChecks.requestProblem(request(outside.path), dir))
        assertEquals("the APK path is not plain", AppInstallChecks.requestProblem(request("${dir.path}/../elsewhere/app.apk"), dir))
        assertEquals("the APK path is not absolute", AppInstallChecks.requestProblem(request("app.apk"), dir))
    }

    @Test fun notAnApkIsRefused() {
        val dir = folder()
        val file = apk(dir, "notes.txt")
        assertEquals("the file is not an .apk file", AppInstallChecks.requestProblem(request(file.path), dir))
        val upper = apk(dir, "APP.APK")
        assertNull(AppInstallChecks.requestProblem(request(upper.path), dir))
    }

    @Test fun linksAreRefused() {
        val dir = folder()
        val target = apk(temp.newFolder("real"))
        val link = File(dir, "link.apk")
        Files.createSymbolicLink(link.toPath(), target.toPath())
        assertEquals("the APK path contains a link", AppInstallChecks.requestProblem(request(link.path), dir))
        val linkedDir = File(dir, "d")
        Files.createSymbolicLink(linkedDir.toPath(), target.parentFile!!.toPath())
        assertEquals("the APK path contains a link", AppInstallChecks.requestProblem(request("${linkedDir.path}/app.apk"), dir))
    }

    @Test fun folderIsNotARegularFile() {
        val dir = folder()
        val sub = File(dir, "x.apk").apply { mkdirs() }
        assertEquals("the APK is not a regular file", AppInstallChecks.requestProblem(request(sub.path), dir))
    }

    @Test fun minionItselfIsRefused() {
        assertEquals("Minion updates itself with its own update path",
            AppInstallChecks.problem(facts(pkg = own), own, null, allowNew = true))
    }

    @Test fun unreadableArchiveIsRefused() {
        assertEquals("Android cannot read the APK", AppInstallChecks.problem(null, own, null, true))
        assertEquals("the APK has no signer", AppInstallChecks.problem(facts(signers = emptyList()), own, null, true))
    }

    @Test fun newInstallNeedsAllowNew() {
        assertTrue(AppInstallChecks.problem(facts(), own, null, allowNew = false)!!.contains("--allow-new"))
        assertNull(AppInstallChecks.problem(facts(), own, null, allowNew = true))
    }

    @Test fun signerMustMatchTheInstalledApp() {
        val installed = InstalledApp(4, listOf(otherKey))
        assertEquals("the APK is signed with another key than the installed app",
            AppInstallChecks.problem(facts(), own, installed, allowNew = true))
        // Order and case do not matter.
        val two = InstalledApp(4, listOf(otherKey, key.uppercase()))
        assertNull(AppInstallChecks.problem(facts(signers = listOf(key, otherKey)), own, two, false))
    }

    @Test fun downgradeIsRefused() {
        val installed = InstalledApp(6, listOf(key))
        assertTrue(AppInstallChecks.problem(facts(code = 5), own, installed, allowNew = true)!!.contains("downgrade"))
    }

    @Test fun sameVersionNeedsAllowNew() {
        val installed = InstalledApp(5, listOf(key))
        assertEquals("already installed at this version; pass --allow-new to reinstall",
            AppInstallChecks.problem(facts(code = 5), own, installed, allowNew = false))
        assertNull(AppInstallChecks.problem(facts(code = 5), own, installed, allowNew = true))
    }

    @Test fun updateWithTheSameSignerIsAccepted() {
        assertNull(AppInstallChecks.problem(facts(code = 7), own, InstalledApp(5, listOf(key)), allowNew = false))
    }

    @Test fun signerTextIsSortedAndLowercase() {
        assertEquals("$key,$otherKey", AppInstallChecks.signerText(listOf(otherKey.uppercase(), key)))
    }
}
