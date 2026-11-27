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
import android.provider.Settings
import android.system.Os
import org.json.JSONObject
import java.io.File
import java.io.FileInputStream
import java.nio.charset.StandardCharsets.UTF_8
import java.security.MessageDigest
import java.time.Instant
import java.time.OffsetDateTime
import java.util.concurrent.Executors

/** What the Minion screen shows about updates. */
internal data class UpdateView(
    val policy: UpdatePolicy = UpdatePolicy.AUTOMATIC,
    val canInstall: Boolean = true,
    val selfSigned: Boolean = true,
    /** The attempt state, "" without a staged update (see [ApkUpdater] for the list). */
    val state: String = "",
    val version: String = "",
    val reason: String = "",
    /** The Install update button: a confirmation waits, or the owner may start the install. */
    val offer: Boolean = false,
    val sessions: Int = -1,
    val lastResult: String = "",
)

/**
 * The Kotlin half of the APK self-update (docs: minion-android-update DESIGN
 * section 2.6 with the review corrections). The Go side is
 * internal/worker/agent/selfupdate_apk.go; files in `<state>/update/`:
 *
 * - In: `apk-pending.json` (`id`, `tag`, `version`, `version_code`, `package`,
 *   `path` in `apk/`, `sha256`, `size`, `now`, `approved`, `expires_at`). The
 *   Worker writes it at an idle moment (or for Update now), holds new sessions
 *   meanwhile and removes it to withdraw. The app must take it (report any
 *   attempt state for its id) before `expires_at`.
 * - Out: `apk-status.json`, rewritten when it changes: `ready` and
 *   `block_reason` (the update block the Hub shows), `app_version_code`, and
 *   `attempt` {`id`, `state`, `reason`, ...}. The Worker ends the update on
 *   `failed`, `refused`, `declined`, `interrupted`, `unknown`, `blocked` or
 *   `off`, allows sessions again on `needs_confirmation_on_device` (also used
 *   while the app waits for the owner: Ask, Android 10/11), and keeps waiting
 *   on `installing`, `installed` and the other waiting states. Outcomes also
 *   go to `apk-history.jsonl` (last 50).
 *
 * Every process of the app's user can rewrite those files, so the request's
 * digest is not trusted alone: `worker apk-verify` (a fresh process of the
 * installed libminion.so) authenticates the staged checksums.txt offline, the
 * bytes streamed into the session must hash to that digest, and the APK must
 * carry the pinned release certificate and a strictly higher version code.
 */
internal class ApkUpdater private constructor(context: Context) {
    private val app = context.applicationContext
    private val work = Executors.newSingleThreadExecutor()
    private val main = Handler(Looper.getMainLooper())
    private val preferences = app.getSharedPreferences("updater", Context.MODE_PRIVATE)
    private val updateDir = File(app.noBackupFilesDir, "state/update")
    private val stageDir = File(updateDir, "apk")
    private val native = NativeWorker(app)

    @Volatile var view = UpdateView()
        private set

    /** Set by the visible Minion screen: launches a confirmation at once. */
    @Volatile var launcher: ((Intent) -> Unit)? = null

    /** Called (any thread) when [view] changed. */
    @Volatile var onChange: (() -> Unit)? = null

    // Owned by the work thread.
    private var attempt: Attempt? = null
    private var liveSessions = -1
    private var active = false
    private var canInstall = true
    private var selfSigned = true
    private var installedCode = 0L
    private var confirmation: Intent? = null
    private var lastStatus = ""
    private var notified = ""
    private var committedAt = 0L
    private var requestPresent = false

    init {
        work.execute { safely { reconcile() } }
    }

    // ---- entry points -------------------------------------------------------

    /** From the Worker service tick (every 4 s): the newest status report. */
    fun poll(report: JSONObject?, active: Boolean) {
        val sessions = report?.optInt("sessions", -1) ?: -1
        work.execute {
            safely {
                this.active = active
                liveSessions = sessions
                evaluate(userRequested = false)
            }
        }
    }

    /** The screen opened or came back from Settings: re-read readiness. */
    fun refresh() = work.execute { safely { evaluate(userRequested = false) } }

    fun setPolicy(policy: UpdatePolicy) = work.execute {
        safely {
            preferences.edit().putString(POLICY, policy.key).commit()
            evaluate(userRequested = false)
        }
    }

    /** The Install update button. */
    fun installNow() = work.execute {
        safely {
            val waiting = confirmation
            if (waiting != null && attempt?.state == NEEDS_CONFIRMATION) {
                launch(waiting)
            } else {
                evaluate(userRequested = true)
            }
        }
    }

    /** The commit result from [InstallStatusReceiver]. */
    fun onStatus(intent: Intent) = work.execute { safely { status(intent) } }

    // ---- state --------------------------------------------------------------

    private fun policy() = UpdatePolicy.of(preferences.getString(POLICY, null))

    private fun pending(): PendingApk? {
        val candidate = readPendingFile() ?: return null
        // Any attempt state written for its id tells the Worker it is taken.
        val taken = attempt?.key == candidate.key
        // Not taken in time: the Worker withdraws it and reports the failure.
        if (UpdateChecks.expired(candidate.expiresAt, System.currentTimeMillis(), taken)) return null
        val problem = UpdateChecks.pendingProblem(candidate, stageDir, app.packageName) ?: return candidate
        val refused = Attempt.of(candidate).copy(state = REFUSED, reason = problem)
        if (attempt?.key != refused.key || attempt?.state != REFUSED) finish(refused, REFUSED, problem)
        return null
    }

    private fun readPendingFile(): PendingApk? {
        val file = File(updateDir, REQUEST_FILE)
        if (!file.isFile || file.length() > 64 * 1024) return null
        return runCatching { parsePending(JSONObject(file.readText(UTF_8))) }.getOrNull()
    }

    private fun parsePending(json: JSONObject): PendingApk? {
        val path = json.optString("path")
        if (path.isBlank()) return null
        val expires = json.optString("expires_at")
        return PendingApk(
            path = path,
            versionCode = json.optLong("version_code", 0),
            sha256 = UpdateChecks.normalizeDigest(json.optString("sha256")),
            version = json.optString("version"),
            tag = json.optString("tag"),
            id = json.optString("id"),
            // The Worker writes a request only at an idle moment or for Update
            // now, with new sessions held: its presence approves the restart.
            approved = json.optBoolean("approved", true),
            packageName = json.optString("package"),
            size = json.optLong("size", 0),
            // Unreadable: treated as already expired unless taken.
            expiresAt = if (expires.isBlank()) 0 else
                runCatching { OffsetDateTime.parse(expires).toInstant().toEpochMilli() }.getOrDefault(1),
        )
    }

    private fun readiness() {
        canInstall = app.packageManager.canRequestPackageInstalls()
        val own = packageInfo()
        installedCode = own?.longVersionCode ?: 0
        selfSigned = UpdateChecks.selfSigned(signers(own), BuildConfig.SIGNING_CERT_SHA256)
    }

    private fun evaluate(userRequested: Boolean) {
        readiness()
        val p = pending()
        requestPresent = p != null
        var a = attempt
        if (p == null) {
            // The Worker withdrew the update (or never staged one): drop a
            // waiting attempt, keep a final outcome for the status screen.
            if (a != null && a.state in WAITING) setAttempt(null)
            publish()
            return
        }
        if (a == null || a.key != p.key) {
            a = Attempt.of(p)
            setAttempt(a)
        }
        if (p.versionCode <= installedCode) {
            if (a.state != INSTALLED) finish(a, INSTALLED, "")
            publish()
            return
        }
        if (a.state == INSTALLING && committedAt > 0 && SystemClock.uptimeMillis() - committedAt > RESULT_TIMEOUT) {
            // The result is lost: no session survives this, try again.
            abandonOwnSessions(app.packageManager.packageInstaller)
            a = a.copy(state = INTERRUPTED, reason = "Android sent no install result", sessionId = -1)
            setAttempt(a)
            committedAt = 0
        }
        if (a.state == INSTALLING || a.state == NEEDS_CONFIRMATION && confirmation != null) {
            publish()
            return
        }
        val final = a.state in FINAL || a.state == INTERRUPTED && a.tries >= UpdateChecks.MAX_TRIES
        if (final && !userRequested) {
            publish()
            return
        }
        if (!active && !userRequested) {
            // A request of a Worker that is not running is withdrawn at its
            // next start; leave it alone meanwhile.
            publish()
            return
        }
        val wait = UpdateChecks.throttleWait(
            preferences.getLong(LAST_SILENT, 0), preferences.getInt(LAST_BOOT, -1), SystemClock.uptimeMillis(), bootCount(),
        )
        val (step, reason) = UpdateChecks.decide(
            policy(), canInstall, selfSigned, Build.VERSION.SDK_INT, liveSessions, p.approved, userRequested, wait,
        )
        if (step == Step.INSTALL) install(p, a, userRequested)
        else setAttempt(a.copy(state = stateOf(step), reason = reason))
        publish()
    }

    private fun stateOf(step: Step) = when (step) {
        Step.OFF -> OFF
        Step.BLOCKED -> BLOCKED
        Step.WAIT_APPROVAL -> WAITING_APPROVAL
        Step.WAIT_IDLE -> WAITING_IDLE
        Step.WAIT_THROTTLE -> WAITING_THROTTLE
        Step.INSTALL -> INSTALLING
    }

    // ---- install ------------------------------------------------------------

    private fun install(p: PendingApk, start: Attempt, userRequested: Boolean) {
        // Reported as installing at once, before the slower checks.
        var a = start.copy(state = INSTALLING, reason = "", tries = start.tries + 1, userRequested = userRequested, sessionId = -1)
        setAttempt(a)
        publish()
        val installer = app.packageManager.packageInstaller
        abandonOwnSessions(installer)
        val file = File(p.path)
        val verified = native.verifyStagedApk(p.tag)
        if (verified == null) {
            finish(a, REFUSED, "the staged APK did not pass the signature check of release ${p.tag}")
            return
        }
        val sameFile = runCatching {
            File(verified.optString("path")).canonicalFile == file.canonicalFile
        }.getOrDefault(false)
        // The digest the bytes must have: the one the signed checksums gave.
        val authenticated = UpdateChecks.normalizeDigest(verified.optString("sha256"))
        val facts = archiveFacts(file)
        UpdateChecks.archiveProblem(facts, p, app.packageName, installedCode, BuildConfig.SIGNING_CERT_SHA256)?.let {
            finish(a, REFUSED, it)
            return
        }
        val silent = Build.VERSION.SDK_INT >= 31
        val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL).apply {
            setAppPackageName(app.packageName)
            setInstallReason(PackageManager.INSTALL_REASON_USER)
            if (silent) setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_NOT_REQUIRED)
        }
        var sessionId = -1
        try {
            FileInputStream(file).use { input ->
                // The size and every byte come from this one open file.
                val length = input.channel.size()
                if (length <= 0 || length > UpdateChecks.MAX_APK_BYTES) {
                    finish(a, REFUSED, "the staged APK has an invalid size ($length bytes)")
                    return
                }
                UpdateChecks.verifiedProblem(
                    p, verified.optString("tag"), verified.optLong("version_code", -1), authenticated,
                    verified.optLong("size", -1), sameFile, length,
                )?.let {
                    finish(a, REFUSED, it)
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
                        check(total == length) { "the staged APK changed while it was read" }
                        session.fsync(out)
                    }
                    if (UpdateChecks.hex(digest.digest()) != authenticated) {
                        session.abandon()
                        finish(a, REFUSED, "the APK does not match its signed checksum")
                        return
                    }
                    // Recheck right before the commit: the record, the setting,
                    // the switch and the sessions may have changed meanwhile.
                    readiness()
                    val again = pending()
                    val wait = UpdateChecks.throttleWait(
                        preferences.getLong(LAST_SILENT, 0), preferences.getInt(LAST_BOOT, -1),
                        SystemClock.uptimeMillis(), bootCount(),
                    )
                    val (step, reason) = UpdateChecks.decide(
                        policy(), canInstall, selfSigned, Build.VERSION.SDK_INT, liveSessions,
                        again?.approved == true, userRequested, wait,
                    )
                    if (again?.key != p.key || step != Step.INSTALL) {
                        session.abandon()
                        val withdrawn = again?.key != p.key
                        setAttempt(a.copy(
                            state = if (withdrawn) INTERRUPTED else stateOf(step),
                            reason = if (withdrawn) "the Worker withdrew the update request" else reason,
                            sessionId = -1, tries = a.tries - 1,
                        ))
                        return
                    }
                    if (silent) {
                        // Android records its throttle before the install ends.
                        preferences.edit().putLong(LAST_SILENT, SystemClock.uptimeMillis()).putInt(LAST_BOOT, bootCount()).commit()
                    }
                    session.commit(statusSender(sessionId).intentSender)
                    committedAt = SystemClock.uptimeMillis()
                }
            }
        } catch (e: Exception) {
            if (sessionId >= 0) runCatching { installer.abandonSession(sessionId) }
            finish(a, FAILED, "could not hand the APK to Android: ${e.message ?: e.javaClass.simpleName}")
        }
    }

    private fun statusSender(sessionId: Int): PendingIntent {
        // Explicit and mutable: Android fills in the status, the session id
        // and the confirmation intent. One request code per session.
        val intent = Intent(app, InstallStatusReceiver::class.java)
            .setAction(InstallStatusReceiver.ACTION)
            .setPackage(app.packageName)
            .putExtra(InstallStatusReceiver.EXTRA_OUR_SESSION, sessionId)
        val mutable = if (Build.VERSION.SDK_INT >= 31) PendingIntent.FLAG_MUTABLE else 0
        return PendingIntent.getBroadcast(app, sessionId, intent, PendingIntent.FLAG_UPDATE_CURRENT or mutable)
    }

    private fun status(intent: Intent) {
        val a = attempt ?: return
        val ours = intent.getIntExtra(InstallStatusReceiver.EXTRA_OUR_SESSION, -1)
        val session = intent.getIntExtra(PackageInstaller.EXTRA_SESSION_ID, -2)
        if (ours < 0 || ours != session || session != a.sessionId) return
        val message = intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE).orEmpty()
        if (!intent.hasExtra(PackageInstaller.EXTRA_STATUS)) {
            setAttempt(a.copy(state = UNKNOWN, reason = "Android sent no install status"))
            publish()
            return
        }
        when (val code = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)) {
            PackageInstaller.STATUS_PENDING_USER_ACTION -> {
                val confirm = confirmIntent(intent)
                if (confirm == null) {
                    runCatching { app.packageManager.packageInstaller.abandonSession(session) }
                    setAttempt(a.copy(state = INTERRUPTED, reason = "Android asked for a confirmation without a screen to show"))
                } else {
                    confirmation = confirm
                    setAttempt(a.copy(state = NEEDS_CONFIRMATION, reason = "confirm the update on this device"))
                    if (a.userRequested) launch(confirm)
                }
            }
            PackageInstaller.STATUS_SUCCESS -> finish(a, INSTALLED, "")
            PackageInstaller.STATUS_FAILURE_ABORTED -> finish(a, DECLINED, "the update was cancelled on this device")
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

    /** At process start (also after the update replaced the app). */
    private fun reconcile() {
        attempt = Attempt.load(preferences)
        readiness()
        val a = attempt
        if (a != null) {
            when {
                a.versionCode <= installedCode && a.state != INSTALLED -> finish(a, INSTALLED, "")
                a.state == INSTALLING || a.state == UNKNOWN ->
                    setAttempt(a.copy(state = INTERRUPTED, reason = "the app restarted during the install", sessionId = -1))
                // The confirmation intent lived in the old process.
                a.state == NEEDS_CONFIRMATION ->
                    setAttempt(a.copy(state = WAITING_APPROVAL, reason = "confirm the update on this device", sessionId = -1))
            }
        }
        abandonOwnSessions(app.packageManager.packageInstaller)
        publish()
    }

    private fun abandonOwnSessions(installer: PackageInstaller) {
        for (info in runCatching { installer.mySessions }.getOrDefault(emptyList())) {
            // Sessions for other apps belong to AppInstaller.
            val pkg = info.appPackageName
            if (pkg == null || pkg == app.packageName) runCatching { installer.abandonSession(info.sessionId) }
        }
        confirmation = null
    }

    private fun finish(a: Attempt, state: String, reason: String) {
        val done = a.copy(state = state, reason = reason, sessionId = -1)
        setAttempt(done)
        confirmation = null
        val result = when (state) {
            INSTALLED -> "Updated to ${a.version.ifBlank { a.versionCode.toString() }}"
            else -> "Update ${a.version.ifBlank { a.versionCode.toString() }} $state${if (reason.isBlank()) "" else ": $reason"}"
        }
        preferences.edit().putString(LAST_RESULT, result).commit()
        history(done)
    }

    private fun setAttempt(value: Attempt?) {
        attempt = value
        if (value == null) Attempt.clear(preferences) else value.save(preferences)
    }

    // ---- reporting ----------------------------------------------------------

    private fun publish() {
        val a = attempt
        val policy = policy()
        // The button needs a request the Worker still holds.
        val offer = requestPresent && a != null && a.state in OFFERED && policy != UpdatePolicy.OFF && canInstall && selfSigned ||
            a?.state == NEEDS_CONFIRMATION && confirmation != null
        val v = UpdateView(
            policy, canInstall, selfSigned, a?.state.orEmpty(), a?.version.orEmpty(), a?.reason.orEmpty(), offer,
            liveSessions, preferences.getString(LAST_RESULT, null).orEmpty(),
        )
        writeStatus(v, a)
        notifyOwner(a)
        if (v != view) {
            view = v
            onChange?.invoke()
        }
    }

    private fun writeStatus(v: UpdateView, a: Attempt?) {
        val block = when {
            v.policy == UpdatePolicy.OFF -> "updates are turned off on this device"
            !v.selfSigned -> "this copy of the app is not signed with the Minion release key; install the release APK by hand"
            !v.canInstall -> "Install unknown apps is not allowed for Minion; allow it once on the device"
            else -> ""
        }
        val json = JSONObject()
            .put("schema", 1)
            .put("ready", block.isEmpty())
            .put("block_reason", block)
            .put("app_version_code", installedCode)
            .put("app_version", runCatching { packageInfo()?.versionName }.getOrNull().orEmpty())
            .put("sdk_int", Build.VERSION.SDK_INT)
            .put("silent", Build.VERSION.SDK_INT >= 31)
            .put("silent_target_sdk", UpdateChecks.silentTargetSdk(Build.VERSION.SDK_INT))
            .put("policy", v.policy.key)
            .put("can_request_installs", v.canInstall)
            .put("self_signed", v.selfSigned)
            .put("last_result", v.lastResult)
        if (a != null) {
            json.put("attempt", JSONObject()
                .put("id", a.id).put("state", workerState(a)).put("reason", a.reason).put("app_state", a.state)
                .put("version_code", a.versionCode).put("version", a.version).put("tag", a.tag)
                .put("sha256", a.sha256).put("tries", a.tries))
        }
        val text = json.toString()
        if (text == lastStatus) return
        runCatching {
            ensureDir()
            json.put("updated_at", Instant.now().toString())
            atomicWrite(File(updateDir, APP_FILE), json.toString() + "\n")
            lastStatus = text
        }
    }

    /**
     * The attempt state the Worker follows: waiting for the owner on this
     * device is needs_confirmation_on_device, so the Worker allows sessions
     * again meanwhile; the other states are reported as they are.
     */
    private fun workerState(a: Attempt): String = if (a.state == WAITING_APPROVAL) NEEDS_CONFIRMATION else a.state

    private fun history(a: Attempt) = runCatching {
        ensureDir()
        val file = File(updateDir, HISTORY_FILE)
        val line = JSONObject()
            .put("at", Instant.now().toString()).put("id", a.id).put("version_code", a.versionCode)
            .put("version", a.version).put("tag", a.tag).put("sha256", a.sha256)
            .put("state", a.state).put("reason", a.reason).put("app_version_code", installedCode).toString()
        val lines = (if (file.isFile && file.length() < 256 * 1024) file.readLines(UTF_8) else emptyList()) + line
        atomicWrite(file, lines.takeLast(50).joinToString("\n", postfix = "\n"))
    }

    private fun ensureDir() {
        if (!updateDir.isDirectory) {
            check(updateDir.mkdirs() || updateDir.isDirectory)
            Os.chmod(updateDir.path, 448) // 0700
        }
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

    private fun notifyOwner(a: Attempt?) {
        val waiting = a != null && (a.state == NEEDS_CONFIRMATION || a.state == WAITING_APPROVAL && active &&
            canInstall && selfSigned && policy() != UpdatePolicy.OFF)
        val key = if (waiting) "${a!!.key}|${a.state}" else ""
        if (key == notified) return
        notified = key
        val manager = app.getSystemService(NotificationManager::class.java) ?: return
        runCatching {
            if (!waiting) {
                manager.cancel(NOTIFICATION)
                return@runCatching
            }
            manager.createNotificationChannel(
                NotificationChannel(CHANNEL, "Minion updates", NotificationManager.IMPORTANCE_DEFAULT)
            )
            // Opens the Minion screen only; the Install update button there
            // starts the confirmation (a background start would be blocked).
            val open = PendingIntent.getActivity(
                app, 2, Intent(app, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
            )
            val version = a!!.version.ifBlank { a.versionCode.toString() }
            manager.notify(NOTIFICATION, Notification.Builder(app, CHANNEL)
                .setSmallIcon(R.drawable.notification_icon)
                .setContentTitle("Minion update $version is ready")
                .setContentText("Open Minion and choose Install update.")
                .setContentIntent(open)
                .setAutoCancel(true)
                .setVisibility(Notification.VISIBILITY_PRIVATE)
                .build())
        }
    }

    // ---- Package Manager ----------------------------------------------------

    @Suppress("DEPRECATION")
    private fun packageInfo(): PackageInfo? = runCatching {
        if (Build.VERSION.SDK_INT >= 33) {
            app.packageManager.getPackageInfo(app.packageName,
                PackageManager.PackageInfoFlags.of(PackageManager.GET_SIGNING_CERTIFICATES.toLong()))
        } else {
            app.packageManager.getPackageInfo(app.packageName, PackageManager.GET_SIGNING_CERTIFICATES)
        }
    }.getOrNull()

    @Suppress("DEPRECATION")
    private fun archiveFacts(file: File): ArchiveFacts? = runCatching {
        val info = if (Build.VERSION.SDK_INT >= 33) {
            app.packageManager.getPackageArchiveInfo(file.path,
                PackageManager.PackageInfoFlags.of(PackageManager.GET_SIGNING_CERTIFICATES.toLong()))
        } else {
            app.packageManager.getPackageArchiveInfo(file.path, PackageManager.GET_SIGNING_CERTIFICATES)
        } ?: return@runCatching null
        ArchiveFacts(
            info.packageName, info.longVersionCode, signers(info),
            info.signingInfo?.hasMultipleSigners() == true, info.applicationInfo?.targetSdkVersion ?: 0,
        )
    }.getOrNull()

    private fun signers(info: PackageInfo?): List<String> {
        // The current signers only, never the rotation history: a key
        // rotation needs a release that pins its successor first.
        val signing = info?.signingInfo ?: return emptyList()
        return signing.apkContentsSigners.orEmpty().map { UpdateChecks.sha256(it.toByteArray()) }
    }

    private fun bootCount(): Int = runCatching {
        Settings.Global.getInt(app.contentResolver, Settings.Global.BOOT_COUNT, -1)
    }.getOrDefault(-1)

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
            // Never take the Worker service down with an update problem.
        }
    }

    // ---- persisted attempt ---------------------------------------------------

    private data class Attempt(
        val id: String,
        val versionCode: Long,
        val sha256: String,
        val version: String,
        val tag: String,
        val state: String,
        val reason: String = "",
        val tries: Int = 0,
        val sessionId: Int = -1,
        val userRequested: Boolean = false,
    ) {
        val key: String get() = "$id|$versionCode|$sha256"

        fun save(p: android.content.SharedPreferences) {
            p.edit().putString("a_id", id).putLong("a_code", versionCode).putString("a_sha", sha256)
                .putString("a_version", version).putString("a_tag", tag).putString("a_state", state)
                .putString("a_reason", reason).putInt("a_tries", tries).putInt("a_session", sessionId)
                .putBoolean("a_user", userRequested).commit()
        }

        companion object {
            fun of(p: PendingApk) = Attempt(p.id, p.versionCode, p.sha256, p.version, p.tag, "new")

            fun load(p: android.content.SharedPreferences): Attempt? {
                val id = p.getString("a_id", null) ?: return null
                return Attempt(
                    id, p.getLong("a_code", 0), p.getString("a_sha", "").orEmpty(), p.getString("a_version", "").orEmpty(),
                    p.getString("a_tag", "").orEmpty(), p.getString("a_state", "").orEmpty(),
                    p.getString("a_reason", "").orEmpty(), p.getInt("a_tries", 0), p.getInt("a_session", -1),
                    p.getBoolean("a_user", false),
                )
            }

            fun clear(p: android.content.SharedPreferences) {
                val edit = p.edit()
                for (k in listOf("a_id", "a_code", "a_sha", "a_version", "a_tag", "a_state", "a_reason", "a_tries", "a_session", "a_user")) {
                    edit.remove(k)
                }
                edit.commit()
            }
        }
    }

    companion object {
        const val POLICY = "policy"
        private const val LAST_SILENT = "last_silent_uptime"
        private const val LAST_BOOT = "last_silent_boot"
        private const val LAST_RESULT = "last_result"
        const val REQUEST_FILE = "apk-pending.json"
        const val APP_FILE = "apk-status.json"
        const val HISTORY_FILE = "apk-history.jsonl"
        private const val CHANNEL = "updates"
        private const val NOTIFICATION = 2
        private const val RESULT_TIMEOUT = 10 * 60_000L

        const val WAITING_APPROVAL = "waiting_approval"
        const val WAITING_IDLE = "waiting_idle"
        const val WAITING_THROTTLE = "waiting_throttle"
        const val BLOCKED = "blocked"
        const val OFF = "off"
        const val INSTALLING = "installing"
        const val NEEDS_CONFIRMATION = "needs_confirmation_on_device"
        const val INSTALLED = "installed"
        const val DECLINED = "declined"
        const val REFUSED = "refused"
        const val FAILED = "failed"
        const val UNKNOWN = "unknown"
        const val INTERRUPTED = "interrupted"

        private val WAITING = setOf("new", WAITING_APPROVAL, WAITING_IDLE, WAITING_THROTTLE, BLOCKED, OFF, INTERRUPTED)
        private val FINAL = setOf(INSTALLED, DECLINED, REFUSED, FAILED)
        private val OFFERED = setOf(WAITING_APPROVAL, WAITING_IDLE, WAITING_THROTTLE, DECLINED, FAILED, INTERRUPTED, UNKNOWN)

        @SuppressLint("StaticFieldLeak") // the application context only
        @Volatile private var instance: ApkUpdater? = null

        fun get(context: Context): ApkUpdater =
            instance ?: synchronized(this) { instance ?: ApkUpdater(context).also { instance = it } }
    }
}

/** Receives the commit result; not exported, reached only by our own mutable PendingIntent. */
class InstallStatusReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != ACTION) return
        ApkUpdater.get(context).onStatus(intent)
    }

    companion object {
        const val ACTION = "cc.nasz.minion.INSTALL_STATUS"
        const val EXTRA_OUR_SESSION = "cc.nasz.minion.extra.SESSION"
    }
}

/**
 * The new version starts here after an update (or an adb install -r).
 * Android grants this broadcast a foreground-service start allowance.
 */
class PackageReplacedReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_MY_PACKAGE_REPLACED) return
        ApkUpdater.get(context) // records the outcome of the attempt
        // Starts whenever Enabled (A124), and a Disabled Worker the update interrupted.
        autostartAfter(context, Autostart.Trigger.PACKAGE_REPLACED)
    }
}
