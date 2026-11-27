package cc.nasz.minion

import android.annotation.SuppressLint
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInfo
import android.content.pm.PackageInstaller
import android.content.pm.PackageManager
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.system.Os
import org.json.JSONObject
import java.io.File
import java.io.FileInputStream
import java.nio.charset.StandardCharsets.UTF_8
import java.security.MessageDigest
import java.time.Instant
import java.time.OffsetDateTime
import java.util.concurrent.Executors

/** What the Minion screen shows about an app install from Minion files. */
internal data class AppInstallView(
    val state: String = "",
    val label: String = "",
    val reason: String = "",
    /** The Install <label> button: Android waits for a confirmation on this device. */
    val offer: Boolean = false,
)

/**
 * Installs another app from the Minion files folder (app_install,
 * app.install.v1). The Go side is internal/worker/agent/appinstall.go; files
 * in `<state>/update/`:
 *
 * - In: `app-install-pending.json` (`id`, `path` inside Minion files,
 *   `rel_path`, `sha256`, `size`, `allow_new`, `created_at`, `expires_at`).
 *   The Worker checked confinement, size and digest and removes the file to
 *   withdraw the request.
 * - Out: `app-install-status.json`: `ready`, `block_reason`, `sdk_int` and the
 *   `attempt` {`id`, `state`, `reason`, `package`, `version_code`,
 *   `version_name`, `label`, `signer_sha256`, `installed_version_code`,
 *   `new_install`}. States: checking, installing,
 *   needs_confirmation_on_device, installed, refused, failed, declined.
 *   Outcomes also go to `app-install-history.jsonl` (last 50).
 *
 * Every process of the app's user can rewrite those files, so the app checks
 * again itself: the path is confined to Minion files, the APK is copied once
 * into the app's private staging folder and that copy must hash to the
 * requested digest (Package Manager reads and the session gets only that
 * copy, so the checks and the install see the same bytes), Minion itself is
 * refused, an installed app needs the same signers and no downgrade, and a new
 * app needs allow_new. Android allows a silent install only for apps whose
 * installer of record is Minion; anything else asks on the device.
 */
internal class AppInstaller private constructor(context: Context) {
    private val app = context.applicationContext
    private val work = Executors.newSingleThreadExecutor()
    private val main = Handler(Looper.getMainLooper())
    private val preferences = app.getSharedPreferences("app_installer", Context.MODE_PRIVATE)
    private val updateDir = File(app.noBackupFilesDir, "state/update")
    private val filesDir = File(NativeWorker(app).minionFilesPath)

    // The private copy of the APK being installed: outside the Minion files
    // folder, which every session's file transfers write to.
    private val stageDir = File(app.noBackupFilesDir, "app-install")
    private val staged = File(stageDir, "staged.apk")

    @Volatile var view = AppInstallView()
        private set

    /** Set by the visible Minion screen: launches a confirmation at once. */
    @Volatile var launcher: ((Intent) -> Unit)? = null

    /** Called (any thread) when [view] changed. */
    @Volatile var onChange: (() -> Unit)? = null

    // Owned by the work thread.
    private var attempt: Attempt? = null
    private var canInstall = true
    private var confirmation: Intent? = null
    private var committedAt = 0L
    private var lastStatus = ""
    private var notified = ""

    init {
        work.execute { safely { reconcile() } }
    }

    // ---- entry points -------------------------------------------------------

    /** From the Worker service tick. */
    fun poll() = work.execute { safely { evaluate() } }

    /** The Install <label> button. */
    fun confirm() = work.execute {
        safely {
            val waiting = confirmation
            if (waiting != null && attempt?.state == NEEDS_CONFIRMATION) launch(waiting)
        }
    }

    /** The commit result from [AppInstallStatusReceiver]. */
    fun onStatus(intent: Intent) = work.execute { safely { status(intent) } }

    // ---- request ------------------------------------------------------------

    private fun readPending(): PendingAppInstall? {
        val file = File(updateDir, REQUEST_FILE)
        if (!file.isFile || file.length() > 64 * 1024) return null
        return runCatching {
            val json = JSONObject(file.readText(UTF_8))
            if (json.optInt("schema", 0) != 1) return null
            val expires = json.optString("expires_at")
            PendingAppInstall(
                id = json.optString("id"),
                path = json.optString("path"),
                relPath = json.optString("rel_path"),
                sha256 = UpdateChecks.normalizeDigest(json.optString("sha256")),
                size = json.optLong("size", 0),
                allowNew = json.optBoolean("allow_new", false),
                // Unreadable: treated as already expired.
                expiresAt = if (expires.isBlank()) 0 else
                    runCatching { OffsetDateTime.parse(expires).toInstant().toEpochMilli() }.getOrDefault(1),
            )
        }.getOrNull()
    }

    private fun evaluate() {
        canInstall = app.packageManager.canRequestPackageInstalls()
        val p = readPending()
        val a = attempt
        if (p != null && (a == null || a.id != p.id)) {
            // Not taken in time: the Worker withdraws it and reports it.
            if (!UpdateChecks.expired(p.expiresAt, System.currentTimeMillis(), taken = false)) start(p)
        } else if (a != null && a.state == INSTALLING && committedAt > 0 &&
            SystemClock.uptimeMillis() - committedAt > RESULT_TIMEOUT) {
            abandonOwnSessions()
            finish(a, FAILED, "Android sent no install result")
        } else if (a != null && a.state !in FINAL && p == null && a.state != NEEDS_CONFIRMATION && committedAt == 0L) {
            // Withdrawn before the commit.
            abandonOwnSessions()
            finish(a, FAILED, "the Worker withdrew the install request")
        }
        publish()
    }

    private fun start(p: PendingAppInstall) {
        var a = Attempt(p.id, CHECKING)
        setAttempt(a)
        publish()
        if (!canInstall) {
            finish(a, REFUSED, BLOCK_INSTALLS)
            return
        }
        AppInstallChecks.requestProblem(p, filesDir)?.let {
            finish(a, REFUSED, it)
            return
        }
        // One copy of the APK, the bytes the Worker hashed, is what is
        // checked, reported and installed.
        val stageProblem = try {
            ensureStageDir()
            AppInstallChecks.stage(File(p.path), p.size, p.sha256, staged)
        } catch (e: Exception) {
            finish(a, FAILED, "could not copy the APK: ${e.message ?: e.javaClass.simpleName}")
            return
        }
        stageProblem?.let {
            finish(a, REFUSED, it)
            return
        }
        val facts = archiveFacts(staged)
        val installed = facts?.packageName?.takeIf { it.isNotBlank() }?.let(::installedApp)
        a = a.copy(
            packageName = facts?.packageName.orEmpty(), versionCode = facts?.versionCode ?: 0,
            versionName = facts?.versionName.orEmpty(), label = facts?.label.orEmpty(),
            signer = facts?.let { AppInstallChecks.signerText(it.signers) }.orEmpty(),
            installedCode = installed?.versionCode ?: 0, newInstall = installed == null,
        )
        setAttempt(a)
        AppInstallChecks.problem(facts, app.packageName, installed, p.allowNew)?.let {
            finish(a, REFUSED, it)
            return
        }
        install(p, a)
    }

    private fun install(p: PendingAppInstall, start: Attempt) {
        var a = start.copy(state = INSTALLING)
        setAttempt(a)
        publish()
        val installer = app.packageManager.packageInstaller
        abandonOwnSessions()
        val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL).apply {
            setAppPackageName(a.packageName)
            setInstallReason(PackageManager.INSTALL_REASON_USER)
            // Silent only when Minion is the installer of record of the
            // package; Android asks on the device otherwise.
            if (Build.VERSION.SDK_INT >= 31) setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_NOT_REQUIRED)
        }
        var sessionId = -1
        try {
            FileInputStream(staged).use { input ->
                // The private copy that was checked; the size and every
                // byte come from this one open file.
                val length = input.channel.size()
                if (length != p.size || length <= 0 || length > AppInstallChecks.MAX_APK_BYTES) {
                    finish(a, REFUSED, AppInstallChecks.CHANGED)
                    return
                }
                params.setSize(length)
                sessionId = installer.createSession(params)
                a = a.copy(sessionId = sessionId)
                setAttempt(a)
                installer.openSession(sessionId).use { session ->
                    val digest = MessageDigest.getInstance("SHA-256")
                    session.openWrite("base.apk", 0, length).use { out ->
                        val buffer = ByteArray(64 * 1024)
                        var total = 0L
                        while (true) {
                            val n = input.read(buffer)
                            if (n < 0) break
                            digest.update(buffer, 0, n)
                            out.write(buffer, 0, n)
                            total += n
                        }
                        check(total == length) { AppInstallChecks.CHANGED_WHILE_READ }
                        session.fsync(out)
                    }
                    if (UpdateChecks.hex(digest.digest()) != p.sha256) {
                        session.abandon()
                        finish(a, REFUSED, AppInstallChecks.DIGEST_MISMATCH)
                        return
                    }
                    // Right before the commit: the Worker may have withdrawn it.
                    if (readPending()?.id != p.id) {
                        session.abandon()
                        finish(a, FAILED, "the Worker withdrew the install request")
                        return
                    }
                    session.commit(statusSender(sessionId).intentSender)
                    committedAt = SystemClock.uptimeMillis()
                }
            }
        } catch (e: Exception) {
            if (sessionId >= 0) runCatching { installer.abandonSession(sessionId) }
            finish(a, FAILED, "could not hand the APK to Android: ${e.message ?: e.javaClass.simpleName}")
        } finally {
            // The session holds its own copy once written.
            staged.delete()
        }
    }

    private fun statusSender(sessionId: Int): PendingIntent {
        // Explicit and mutable: Android fills in the status, the session id
        // and the confirmation intent. One request code per session.
        val intent = Intent(app, AppInstallStatusReceiver::class.java)
            .setAction(AppInstallStatusReceiver.ACTION)
            .setPackage(app.packageName)
            .putExtra(AppInstallStatusReceiver.EXTRA_OUR_SESSION, sessionId)
        val mutable = if (Build.VERSION.SDK_INT >= 31) PendingIntent.FLAG_MUTABLE else 0
        return PendingIntent.getBroadcast(app, sessionId, intent, PendingIntent.FLAG_UPDATE_CURRENT or mutable)
    }

    private fun status(intent: Intent) {
        val a = attempt ?: return
        val ours = intent.getIntExtra(AppInstallStatusReceiver.EXTRA_OUR_SESSION, -1)
        val session = intent.getIntExtra(PackageInstaller.EXTRA_SESSION_ID, -2)
        if (ours < 0 || ours != session || session != a.sessionId) return
        committedAt = 0
        val message = intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE).orEmpty()
        when (val code = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)) {
            PackageInstaller.STATUS_PENDING_USER_ACTION -> {
                val confirm = confirmIntent(intent)
                if (confirm == null) {
                    runCatching { app.packageManager.packageInstaller.abandonSession(session) }
                    finish(a, FAILED, "Android asked for a confirmation without a screen to show")
                } else {
                    confirmation = confirm
                    setAttempt(a.copy(state = NEEDS_CONFIRMATION, reason = "confirm the install on this device"))
                }
            }
            PackageInstaller.STATUS_SUCCESS -> finish(a, INSTALLED, "")
            PackageInstaller.STATUS_FAILURE_ABORTED -> finish(a, DECLINED, "the install was cancelled on this device")
            else -> finish(a, FAILED, "${statusName(code)}${if (message.isBlank()) "" else ": $message"}")
        }
        publish()
    }

    @Suppress("DEPRECATION")
    private fun confirmIntent(intent: Intent): Intent? =
        if (Build.VERSION.SDK_INT >= 33) intent.getParcelableExtra(Intent.EXTRA_INTENT, Intent::class.java)
        else intent.getParcelableExtra(Intent.EXTRA_INTENT)

    private fun launch(confirm: Intent) {
        val target = launcher ?: return
        main.post { runCatching { target(confirm) } }
    }

    /** At process start: an attempt of an earlier process cannot finish any more. */
    private fun reconcile() {
        attempt = Attempt.load(preferences)
        canInstall = app.packageManager.canRequestPackageInstalls()
        val a = attempt
        if (a != null && a.state !in FINAL) {
            finish(a, FAILED, if (a.state == NEEDS_CONFIRMATION)
                "the app restarted before the install was confirmed; run the install again"
            else "the app restarted during the install; run the install again")
        }
        staged.delete()
        abandonOwnSessions()
        publish()
    }

    /** Abandons this installer's sessions only: those for another package than Minion. */
    private fun abandonOwnSessions() {
        val installer = app.packageManager.packageInstaller
        for (info in runCatching { installer.mySessions }.getOrDefault(emptyList())) {
            val pkg = info.appPackageName
            if (pkg != null && pkg != app.packageName) runCatching { installer.abandonSession(info.sessionId) }
        }
        confirmation = null
    }

    private fun finish(a: Attempt, state: String, reason: String) {
        val done = a.copy(state = state, reason = reason, sessionId = -1)
        staged.delete()
        setAttempt(done)
        confirmation = null
        committedAt = 0
        history(done)
    }

    private fun setAttempt(value: Attempt?) {
        attempt = value
        if (value == null) Attempt.clear(preferences) else value.save(preferences)
    }

    // ---- reporting ----------------------------------------------------------

    private fun publish() {
        val a = attempt
        val v = AppInstallView(
            a?.state.orEmpty(), a?.let { it.label.ifBlank { it.packageName } }.orEmpty(), a?.reason.orEmpty(),
            a?.state == NEEDS_CONFIRMATION && confirmation != null,
        )
        writeStatus(a)
        notifyOwner(v)
        if (v != view) {
            view = v
            onChange?.invoke()
        }
    }

    private fun writeStatus(a: Attempt?) {
        val block = if (canInstall) "" else BLOCK_INSTALLS
        val json = JSONObject()
            .put("schema", 1)
            .put("ready", block.isEmpty())
            .put("block_reason", block)
            .put("sdk_int", Build.VERSION.SDK_INT)
        if (a != null) {
            json.put("attempt", JSONObject()
                .put("id", a.id).put("state", a.state).put("reason", a.reason).put("package", a.packageName)
                .put("version_code", a.versionCode).put("version_name", a.versionName).put("label", a.label)
                .put("signer_sha256", a.signer).put("installed_version_code", a.installedCode)
                .put("new_install", a.newInstall))
        }
        val text = json.toString()
        if (text == lastStatus) return
        runCatching {
            ensureDir()
            json.put("updated_at", Instant.now().toString())
            atomicWrite(File(updateDir, STATUS_FILE), json.toString() + "\n")
            lastStatus = text
        }
    }

    private fun history(a: Attempt) = runCatching {
        ensureDir()
        val file = File(updateDir, HISTORY_FILE)
        val line = JSONObject()
            .put("at", Instant.now().toString()).put("id", a.id).put("package", a.packageName)
            .put("version_code", a.versionCode).put("version_name", a.versionName).put("signer_sha256", a.signer)
            .put("installed_version_code", a.installedCode).put("new_install", a.newInstall)
            .put("state", a.state).put("reason", a.reason).toString()
        val lines = (if (file.isFile && file.length() < 256 * 1024) file.readLines(UTF_8) else emptyList()) + line
        atomicWrite(file, lines.takeLast(50).joinToString("\n", postfix = "\n"))
    }

    private fun ensureDir() {
        if (!updateDir.isDirectory) {
            check(updateDir.mkdirs() || updateDir.isDirectory)
            Os.chmod(updateDir.path, 448) // 0700
        }
    }

    private fun ensureStageDir() {
        if (!stageDir.isDirectory) check(stageDir.mkdirs() || stageDir.isDirectory)
        Os.chmod(stageDir.path, 448) // 0700
    }

    private fun atomicWrite(target: File, text: String) {
        val temp = File.createTempFile(".${target.name}", ".tmp", target.parentFile)
        try {
            temp.writeText(text, UTF_8)
            Os.chmod(temp.path, 384) // 0600
            check(temp.renameTo(target))
        } finally {
            temp.delete()
        }
    }

    private fun notifyOwner(v: AppInstallView) {
        val key = if (v.offer) "${attempt?.id}|${v.state}" else ""
        if (key == notified) return
        notified = key
        val manager = app.getSystemService(NotificationManager::class.java) ?: return
        runCatching {
            if (!v.offer) {
                manager.cancel(NOTIFICATION)
                return@runCatching
            }
            manager.createNotificationChannel(
                NotificationChannel(CHANNEL, "Minion app installs", NotificationManager.IMPORTANCE_DEFAULT)
            )
            // Opens the Minion screen only; its Install button starts the
            // confirmation (a background activity start would be blocked).
            val open = PendingIntent.getActivity(
                app, 3, Intent(app, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
            )
            manager.notify(NOTIFICATION, Notification.Builder(app, CHANNEL)
                .setSmallIcon(R.drawable.notification_icon)
                .setContentTitle("Install ${v.label}?")
                .setContentText("Open Minion and choose Install ${v.label}.")
                .setContentIntent(open)
                .setAutoCancel(true)
                .setVisibility(Notification.VISIBILITY_PRIVATE)
                .build())
        }
    }

    // ---- Package Manager ----------------------------------------------------

    @Suppress("DEPRECATION")
    private fun archiveFacts(file: File): AppArchiveFacts? = runCatching {
        val pm = app.packageManager
        val info = if (Build.VERSION.SDK_INT >= 33) {
            pm.getPackageArchiveInfo(file.path, PackageManager.PackageInfoFlags.of(PackageManager.GET_SIGNING_CERTIFICATES.toLong()))
        } else {
            pm.getPackageArchiveInfo(file.path, PackageManager.GET_SIGNING_CERTIFICATES)
        } ?: return@runCatching null
        val label = runCatching {
            info.applicationInfo?.let {
                it.sourceDir = file.path
                it.publicSourceDir = file.path
                pm.getApplicationLabel(it).toString()
            }
        }.getOrNull().orEmpty().take(100)
        AppArchiveFacts(info.packageName, info.longVersionCode, info.versionName.orEmpty().take(100),
            label.ifBlank { info.packageName.orEmpty() }, signers(info))
    }.getOrNull()

    @Suppress("DEPRECATION")
    private fun installedApp(packageName: String): InstalledApp? = runCatching {
        val info = if (Build.VERSION.SDK_INT >= 33) {
            app.packageManager.getPackageInfo(packageName,
                PackageManager.PackageInfoFlags.of(PackageManager.GET_SIGNING_CERTIFICATES.toLong()))
        } else {
            app.packageManager.getPackageInfo(packageName, PackageManager.GET_SIGNING_CERTIFICATES)
        }
        InstalledApp(info.longVersionCode, signers(info))
    }.getOrNull()

    private fun signers(info: PackageInfo?): List<String> {
        val signing = info?.signingInfo ?: return emptyList()
        return signing.apkContentsSigners.orEmpty().map { UpdateChecks.sha256(it.toByteArray()) }
    }

    private fun statusName(code: Int) = when (code) {
        PackageInstaller.STATUS_FAILURE_BLOCKED -> "blocked by Android or a device policy"
        PackageInstaller.STATUS_FAILURE_CONFLICT -> "conflicts with the installed app (signer or version)"
        PackageInstaller.STATUS_FAILURE_INCOMPATIBLE -> "incompatible with this device"
        PackageInstaller.STATUS_FAILURE_INVALID -> "the APK is invalid"
        PackageInstaller.STATUS_FAILURE_STORAGE -> "not enough storage"
        else -> "install failed (status $code)"
    }

    private fun safely(action: () -> Unit) {
        try {
            action()
        } catch (_: Exception) {
            // Never take the Worker service down with an install problem.
        }
    }

    // ---- persisted attempt ---------------------------------------------------

    private data class Attempt(
        val id: String,
        val state: String,
        val reason: String = "",
        val packageName: String = "",
        val versionCode: Long = 0,
        val versionName: String = "",
        val label: String = "",
        val signer: String = "",
        val installedCode: Long = 0,
        val newInstall: Boolean = false,
        val sessionId: Int = -1,
    ) {
        fun save(p: android.content.SharedPreferences) {
            p.edit().putString("id", id).putString("state", state).putString("reason", reason)
                .putString("package", packageName).putLong("code", versionCode).putString("version", versionName)
                .putString("label", label).putString("signer", signer).putLong("installed", installedCode)
                .putBoolean("new", newInstall).putInt("session", sessionId).commit()
        }

        companion object {
            fun load(p: android.content.SharedPreferences): Attempt? {
                val id = p.getString("id", null) ?: return null
                return Attempt(
                    id, p.getString("state", "").orEmpty(), p.getString("reason", "").orEmpty(),
                    p.getString("package", "").orEmpty(), p.getLong("code", 0), p.getString("version", "").orEmpty(),
                    p.getString("label", "").orEmpty(), p.getString("signer", "").orEmpty(), p.getLong("installed", 0),
                    p.getBoolean("new", false), p.getInt("session", -1),
                )
            }

            fun clear(p: android.content.SharedPreferences) {
                p.edit().clear().commit()
            }
        }
    }

    companion object {
        const val REQUEST_FILE = "app-install-pending.json"
        const val STATUS_FILE = "app-install-status.json"
        const val HISTORY_FILE = "app-install-history.jsonl"
        private const val CHANNEL = "app_installs"
        private const val NOTIFICATION = 3
        private const val RESULT_TIMEOUT = 10 * 60_000L
        private const val BLOCK_INSTALLS = "Install unknown apps is not allowed for Minion; allow it once on the device"

        const val CHECKING = "checking"
        const val INSTALLING = "installing"
        const val NEEDS_CONFIRMATION = "needs_confirmation_on_device"
        const val INSTALLED = "installed"
        const val REFUSED = "refused"
        const val FAILED = "failed"
        const val DECLINED = "declined"
        private val FINAL = setOf(INSTALLED, REFUSED, FAILED, DECLINED)

        @SuppressLint("StaticFieldLeak") // the application context only
        @Volatile private var instance: AppInstaller? = null

        fun get(context: Context): AppInstaller =
            instance ?: synchronized(this) { instance ?: AppInstaller(context).also { instance = it } }
    }
}

/** Receives the commit result of an app install; not exported, reached only by our own mutable PendingIntent. */
class AppInstallStatusReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != ACTION) return
        AppInstaller.get(context).onStatus(intent)
    }

    companion object {
        const val ACTION = "cc.nasz.minion.APP_INSTALL_STATUS"
        const val EXTRA_OUR_SESSION = "cc.nasz.minion.extra.APP_SESSION"
    }
}
