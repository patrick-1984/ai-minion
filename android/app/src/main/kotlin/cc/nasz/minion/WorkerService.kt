package cc.nasz.minion

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.SharedPreferences
import android.content.pm.ServiceInfo
import android.os.Binder
import android.os.Build
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.os.SystemClock
import cc.nasz.minion.devicecontrol.DeviceControl
import org.json.JSONObject
import java.time.Instant
import java.util.concurrent.ScheduledThreadPoolExecutor
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

internal data class WorkerView(
    val wanted: Boolean = false,
    val busy: Boolean = false,
    val status: String = "Stopped",
    val message: String = "",
    val hub: String = "",
    val workerWords: String = "",
    val hubWords: String = "",
    val paired: Boolean = false,
    val sessions: String = "",
    val share: Boolean = true,
    val boot: Boolean = false,
    val code: String = "",
    val expires: String = "",
    val usage: UsageRegistration? = null,
    val dialOverride: String = "",
    val update: UpdateView = UpdateView(),
    // "" unless device control is compiled in and enabled; else its status line (design 4.7).
    val deviceControl: String = "",
)

internal fun workerPreferences(context: Context): SharedPreferences =
    context.getSharedPreferences("supervisor", Context.MODE_PRIVATE)

/** Enabled (start after restarts): true unless the person chose otherwise. */
internal fun autostartEnabled(preferences: SharedPreferences): Boolean =
    Autostart.enabled(if (preferences.contains(Autostart.ENABLED)) preferences.getBoolean(Autostart.ENABLED, true) else null)

/**
 * A device restart or an app install/update: an earlier Stop is over, and the
 * Worker starts when Autostart says so (it starts only with a Hub set up).
 */
internal fun autostartAfter(context: Context, trigger: Autostart.Trigger) {
    val preferences = workerPreferences(context)
    val running = preferences.getBoolean(Autostart.WANTED, false)
    val edit = preferences.edit().remove(Autostart.STOPPED_BY_USER)
    // Nothing runs after a boot; a stale intent must not start a Disabled Worker later.
    if (trigger == Autostart.Trigger.BOOT) edit.putBoolean(Autostart.WANTED, false)
    edit.commit()
    if (!Autostart.shouldStart(trigger, autostartEnabled(preferences), running, stoppedByUser = false)) return
    try {
        context.startForegroundService(Intent(context, WorkerService::class.java).setAction(WorkerService.AUTOSTART))
    } catch (_: IllegalStateException) {
        // A restricted app may have to be opened; opening it starts an Enabled Worker.
    }
}

class WorkerService : Service() {
    private val queue = ScheduledThreadPoolExecutor(1)
    private val main = Handler(Looper.getMainLooper())
    private val stopping = AtomicBoolean()
    private lateinit var native: NativeWorker
    private lateinit var preferences: SharedPreferences
    private lateinit var updater: ApkUpdater
    private var worker: java.lang.Process? = null
    private var tmux: java.lang.Process? = null

    // The executor owns child handles, restart timing, and the persisted
    // intent to run. Only cancellation of a transient process crosses it.
    private var wanted = false
    private var busy = false
    private var notice = ""
    private var report: JSONObject? = null
    private var usage: UsageRegistration? = null
    private var nextUsageReport = 0L
    private var restartAt = 0L
    private var nextReport = 0L
    private var backoff = 1L
    private var healthySince = 0L
    private var expectingTmux = false
    private var tmuxStartedAt = 0L

    // The Android main thread owns the private Binder observer.
    private val binder = LocalBinder()
    private var listener: ((WorkerView) -> Unit)? = null
    private var view = WorkerView()
    private var foreground = false
    private var requestId = 0L

    internal inner class LocalBinder : Binder() {
        fun observe(observer: ((WorkerView) -> Unit)?) {
            listener = observer
            observer?.invoke(view)
            if (observer != null) queue.execute {
                nextReport = 0
                nextUsageReport = 0
            }
        }

        fun start() = launch(null, null, "all")
        // dialOverride is "hostname=address", or "" for none; it replaces the saved one.
        fun join(token: String, share: String, acceptUsage: Boolean, dialOverride: String) =
            launch(null, token, share, acceptUsage, dialOverride)
        fun pair(server: String, share: String, acceptUsage: Boolean, dialOverride: String) =
            launch(server, null, share, acceptUsage, dialOverride)
        fun stop() = stopWorker()
        fun pause(paused: Boolean) = runCommand(if (paused) "resume" else "pause")
        fun share(share: Boolean) {
            // Design 5: device control needs a dedicated Worker; sharing is refused while it is enabled.
            if (share && DeviceControl.available && DeviceControl.get(this@WorkerService).enabled) {
                queue.execute {
                    notice = "Disable Device control before sharing this Worker's terminal sessions."
                    nextReport = 0
                    publish()
                }
                return
            }
            runCommand("config", "share", if (share) "all" else "none")
        }
        // Enable / Disable: whether the Worker starts after restarts. Enable also
        // starts a stopped Worker now; Disable leaves a running one running.
        fun autostart(enabled: Boolean) {
            queue.execute {
                if (!preferences.edit().putBoolean(Autostart.ENABLED, enabled).commit()) {
                    notice = "Android could not save the change. Try again."
                }
                publish()
            }
            if (enabled && !view.wanted && !view.busy && view.hub.isNotBlank()) launch(null, null, "all")
        }

        // The screen opened: an Enabled, set-up Worker that is not running and
        // was not stopped with Stop since the last restart starts now (A124).
        fun opened() {
            val id = requestId
            queue.execute {
                if (wanted || busy) return@execute
                val start = Autostart.shouldStart(
                    Autostart.Trigger.APP_OPENED, autostartEnabled(preferences), running = false,
                    stoppedByUser = preferences.getBoolean(Autostart.STOPPED_BY_USER, false),
                )
                if (!start) return@execute
                val configured = runCatching { !native.report()?.optString("hub_url").isNullOrBlank() }.getOrDefault(false)
                // A Stop or Start pressed meanwhile wins.
                if (configured) main.post { if (id == requestId) launch(null, null, "all") }
            }
        }

        fun acceptUsage(registration: UsageRegistration) {
            queue.execute {
                if (busy) return@execute
                val accepted = runCatching { native.acceptUsage(registration) }.getOrDefault(false)
                notice = if (accepted) "" else "Could not accept usage monitoring. Review this enrollment and try again."
                nextReport = 0
                nextUsageReport = 0
                tick()
            }
        }

        // Updates: Automatic / Ask / Off. Off is also the Worker's local veto
        // (remote_update=off, read at its next start); the app enforces it at once.
        fun updatePolicy(policy: UpdatePolicy) {
            updater.setPolicy(policy)
            queue.execute {
                if (busy) return@execute
                val value = if (policy == UpdatePolicy.OFF) "off" else "on"
                val saved = runCatching { native.workerCommand("config", "set", "remote_update", value) }.getOrDefault(false)
                if (!saved && !report?.optString("hub_url").isNullOrBlank()) {
                    notice = "The Worker could not save the update setting. The app still applies it."
                }
                publish()
            }
        }

        fun installUpdate() = updater.installNow()

        // Takes effect on the next start; only offered while the Worker is stopped.
        fun clearDialOverride() {
            queue.execute {
                if (wanted || busy) return@execute
                if (!preferences.edit().remove(HubDialOverride.PREFERENCE).commit()) {
                    notice = "Android could not save the change. Try again."
                }
                publish()
            }
        }

        fun forget() {
            queue.execute {
                if (wanted || busy) return@execute
                val ok = runCatching { native.workerCommand("reset", "--yes") &&
                    native.workerCommand("config", "unset", "hub_fingerprint") &&
                    native.workerCommand("resume") }.getOrDefault(false)
                // The override belongs to the forgotten Hub's hostname.
                if (ok) preferences.edit().remove(HubDialOverride.PREFERENCE).commit()
                notice = if (ok) "" else "Could not forget the Hub. Try again."
                report = null
                usage = null
                nextReport = 0
                nextUsageReport = 0
                publish()
            }
        }
    }

    override fun onCreate() {
        super.onCreate()
        native = NativeWorker(this)
        preferences = workerPreferences(this)
        updater = ApkUpdater.get(this)
        updater.onChange = { queue.execute { publish() } }
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel(CHANNEL, "Minion Worker", NotificationManager.IMPORTANCE_LOW)
        )
        queue.scheduleWithFixedDelay({ tick() }, 0, 1, TimeUnit.SECONDS)
    }

    override fun onBind(intent: Intent?): IBinder = binder

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == STOP) {
            stopWorker()
            return START_NOT_STICKY
        }
        showForeground("Starting")
        if (intent?.action == AUTOSTART) {
            // After a boot or an app update (autostartAfter decided): start when a Hub is set up.
            val id = requestId
            queue.execute {
                if (wanted || busy) return@execute
                val latest = if (stopping.get()) null else runCatching { native.report() }.getOrNull()
                if (latest?.optString("hub_url").isNullOrBlank()) {
                    // No Hub set up yet (or stopped meanwhile): nothing to start, quietly.
                    main.post { if (id == requestId && foreground) leaveForeground() }
                } else {
                    report = latest
                    safely { restore() }
                }
            }
        } else if (intent?.action != UI_START) {
            queue.execute {
                if (!stopping.get() && preferences.getBoolean(Autostart.WANTED, false)) {
                    safely { restore() }
                } else {
                    main.post { leaveForeground() }
                }
            }
        }
        return START_STICKY
    }

    private fun launch(
        server: String?, token: String?, share: String, acceptUsage: Boolean = false, dialOverride: String? = null,
    ) {
        ++requestId
        stopping.set(false)
        try {
            startForegroundService(Intent(this, WorkerService::class.java).setAction(UI_START))
        } catch (_: IllegalStateException) {
            view = view.copy(message = "Android blocked background startup. Open Minion Worker and try again.")
            listener?.invoke(view)
            return
        }
        queue.execute {
            if (stopping.get() || wanted || busy) return@execute
            safely {
                busy = true
                notice = ""
                publish()
                if (server != null || token != null) {
                    // Saved before provisioning: the join and the pairing dial already use it.
                    val edit = preferences.edit()
                    if (dialOverride.isNullOrBlank()) edit.remove(HubDialOverride.PREFERENCE)
                    else edit.putString(HubDialOverride.PREFERENCE, dialOverride)
                    check(edit.commit())
                    saveWanted(true)
                    val completed = runCatching { native.provision(server, token, share, acceptUsage) }.getOrDefault(false)
                    if (stopping.get()) return@safely
                    report = native.report()
                    val configured = !report?.optString("hub_url").isNullOrBlank()
                    val ready = if (token != null) configured && report?.optBoolean("paired") == true
                        else completed && configured
                    if (!ready) {
                        decline("Could not set up the Worker. Check the Hub address or join token and try again.")
                        return@safely
                    }
                }
                restore()
            }
        }
    }

    private fun restore() {
        report = native.report()
        if (report?.optString("hub_url").isNullOrBlank()) {
            decline("Set up a Hub connection before starting the Worker.")
            return
        }
        if (stopping.get()) return
        saveWanted(true)
        busy = false
        backoff = 1
        restartAt = 0
        nextReport = 0
        publish()
    }

    private fun saveWanted(value: Boolean) {
        val edit = preferences.edit().putBoolean(Autostart.WANTED, value)
        if (value) edit.remove(Autostart.STOPPED_BY_USER)
        check(edit.commit())
        wanted = value
    }

    private fun decline(message: String) {
        wanted = false
        busy = false
        notice = message
        preferences.edit().putBoolean(Autostart.WANTED, false).commit()
        publish()
        main.post { if (foreground) leaveForeground() }
    }

    private fun stopWorker() {
        val id = ++requestId
        stopping.set(true)
        native.cancelCommand()
        queue.execute {
            // Stop holds until the next restart or update (or Start / Enable); Enabled stays.
            val saved = preferences.edit().putBoolean(Autostart.WANTED, false)
                .putBoolean(Autostart.STOPPED_BY_USER, true).commit()
            wanted = false
            busy = false
            restartAt = 0
            notice = if (saved) "" else "The Worker stopped. Android could not save the Stop preference."
            try {
                stopChildren()
            } catch (_: Exception) {
                notice = "The Worker could not confirm that all terminal processes stopped."
            } finally {
                report?.remove("sessions")
                nextReport = 0
                publish()
                main.post { if (id == requestId) leaveForeground() }
            }
        }
    }

    private fun stopChildren() {
        try {
            NativeWorker.reap(worker)
        } finally {
            worker = null
            runCatching { native.stopBridge() }
            NativeWorker.reap(tmux)
            tmux = null
            expectingTmux = false
        }
    }

    private fun runCommand(vararg args: String) {
        queue.execute {
            if (busy || stopping.get() && wanted) return@execute
            safely {
                if (!native.workerCommand(*args)) notice = "The Worker could not apply the change. Try again."
                nextReport = 0
                tick()
            }
        }
    }

    private fun tick() {
        if (busy || stopping.get() && wanted) return
        safely {
            val now = SystemClock.elapsedRealtime()
            if (wanted && !stopping.get()) supervise(now)
            if (now >= nextReport) {
                report = native.report()
                nextReport = now + 4_000
                updater.poll(report, wanted && !busy && !stopping.get() && worker?.isAlive == true)
                if (report?.optBoolean("paired") != true) {
                    usage = null
                    nextUsageReport = 0
                } else if (now >= nextUsageReport) {
                    usage = runCatching { native.usageRegistration() }.getOrNull()
                    nextUsageReport = now + 12_000
                }
            }
            publish()
        }
    }

    private fun supervise(now: Long) {
        if (tmux?.isAlive != true) {
            if (expectingTmux) notice = "The terminal server stopped. Its sessions ended."
            if (worker != null || tmux != null) {
                stopChildren()
                retry(now)
            }
            if (now < restartAt) return
            tmux = native.startTmux()
            expectingTmux = true
            tmuxStartedAt = now
            healthySince = 0
            return
        }
        if (worker?.isAlive == true) {
            if (now - healthySince > 30_000) backoff = 1
            return
        }
        if (worker != null) {
            NativeWorker.reap(worker)
            worker = null
            notice = "The Worker disconnected. Its terminal sessions are still running."
            retry(now)
        }
        if (now < restartAt || stopping.get()) return
        if (!native.tmuxReady()) {
            if (now - tmuxStartedAt > 15_000) {
                stopChildren()
                notice = "The terminal server did not become ready. Retrying."
                retry(now)
            }
            return
        }
        worker = native.startWorker()
        healthySince = now
        nextReport = 0
    }

    private fun retry(now: Long) {
        restartAt = now + backoff * 1_000
        backoff = minOf(backoff * 2, 30)
    }

    private fun safely(action: () -> Unit) {
        try {
            action()
        } catch (_: Exception) {
            if (busy) {
                decline("The Android Worker could not start. Try again.")
            } else if (wanted && !stopping.get()) {
                notice = "The Worker could not start its native processes. Retrying."
                retry(SystemClock.elapsedRealtime())
            } else if (!stopping.get()) {
                notice = "The Android Worker status is unavailable."
            }
            publish()
        }
    }

    private fun publish() {
        val r = report
        val connection = r?.optJSONObject("connection")
        val pairing = if (wanted && !busy && !stopping.get() && worker?.isAlive == true) native.pairing() else null
        val expires = pairing?.optString("expires_at").orEmpty()
        val livePairing = pairing?.takeIf { runCatching { Instant.now().isBefore(Instant.parse(expires)) }.getOrDefault(false) }
        val paused = r?.optBoolean("paused") == true
        val status = when {
            busy -> "Setting up"
            !wanted -> "Stopped"
            paused -> "Paused"
            r?.optString("alarm") == "hub_identity_changed" -> "Hub identity changed"
            livePairing != null -> "Ready to pair"
            usage?.let { it.required && !it.accepted } == true -> "Usage acceptance required"
            worker?.isAlive != true -> "Starting"
            connection?.optString("state") == "connected" ->
                if (r.optString("approval") == "pending") "Awaiting Hub approval" else "Connected"
            else -> "Connecting to the Hub"
        }
        val workerWords = livePairing?.optString("worker_words").orEmpty().ifBlank {
            r?.optString("worker_fingerprint_words").orEmpty()
        }
        val hubWords = livePairing?.optString("hub_words_presented").orEmpty().ifBlank {
            livePairing?.optString("hub_words_expected").orEmpty().ifBlank {
                r?.optString("hub_fingerprint_words").orEmpty()
            }
        }
        val share = r?.optJSONObject("share")
        if (DeviceControl.available) {
            // Fail closed: a share change while enabled disables device control and closes its streams.
            DeviceControl.get(this).observeShare(if (worker?.isAlive == true) share?.optString("mode") else null)
        }
        val v = WorkerView(
            wanted, busy, status, notice, r?.optString("hub_url").orEmpty(), workerWords, hubWords,
            r?.optBoolean("paired") == true, r?.optInt("sessions", -1)?.takeIf { it >= 0 }?.toString().orEmpty(),
            share?.optString("mode") != "none", autostartEnabled(preferences),
            livePairing?.optString("code").orEmpty(), if (livePairing != null) expires else "",
            usage, preferences.getString(HubDialOverride.PREFERENCE, null).orEmpty(), updater.view,
            if (DeviceControl.available && DeviceControl.get(this).enabled) DeviceControl.get(this).statusLine() else "",
        )
        main.post {
            if (v != view) {
                val changed = view.status != v.status || view.deviceControl != v.deviceControl
                view = v
                listener?.invoke(v)
                if (foreground && changed) {
                    getSystemService(NotificationManager::class.java).notify(NOTIFICATION, notification(v.status))
                }
            }
        }
    }

    private fun showForeground(status: String) {
        val note = notification(status)
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION, note, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTIFICATION, note)
        }
        foreground = true
    }

    private fun notification(status: String): Notification {
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        val stop = PendingIntent.getService(
            this, 1, Intent(this, WorkerService::class.java).setAction(STOP), PendingIntent.FLAG_IMMUTABLE
        )
        return Notification.Builder(this, CHANNEL)
            .setSmallIcon(R.drawable.notification_icon)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(
                view.deviceControl.ifEmpty { null }?.let { "$status · $it" } ?: status
            )
            .setContentIntent(open)
            .setOngoing(true)
            .setVisibility(Notification.VISIBILITY_PRIVATE)
            .addAction(Notification.Action.Builder(null, "Stop Worker", stop).build())
            .build()
    }

    private fun leaveForeground() {
        foreground = false
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    override fun onDestroy() {
        listener = null
        updater.onChange = null
        stopping.set(true)
        native.cancelCommand()
        queue.execute {
            try {
                stopChildren()
            } finally {
                queue.shutdown()
            }
        }
        super.onDestroy()
    }

    companion object {
        private const val CHANNEL = "worker"
        private const val NOTIFICATION = 1
        private const val STOP = "cc.nasz.minion.STOP"
        private const val UI_START = "cc.nasz.minion.UI_START"
        internal const val AUTOSTART = "cc.nasz.minion.AUTOSTART"
    }
}

// BOOT_COMPLETED only: the app's settings live in credential-encrypted
// storage, which LOCKED_BOOT_COMPLETED (before the first unlock) cannot read.
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        autostartAfter(context, Autostart.Trigger.BOOT)
    }
}
