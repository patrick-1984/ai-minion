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
)

internal fun workerPreferences(context: Context): SharedPreferences =
    context.getSharedPreferences("supervisor", Context.MODE_PRIVATE)

class WorkerService : Service() {
    private val queue = ScheduledThreadPoolExecutor(1)
    private val main = Handler(Looper.getMainLooper())
    private val stopping = AtomicBoolean()
    private lateinit var native: NativeWorker
    private lateinit var preferences: SharedPreferences
    private var worker: java.lang.Process? = null
    private var tmux: java.lang.Process? = null

    // The executor owns child handles, restart timing, and the persisted
    // intent to run. Only cancellation of a transient process crosses it.
    private var wanted = false
    private var busy = false
    private var notice = ""
    private var report: JSONObject? = null
    private var restartAt = 0L
    private var nextReport = 0L
    private var backoff = 1L
    private var healthySince = 0L
    private var expectingTmux = false

    // The Android main thread owns the private Binder observer.
    private val binder = LocalBinder()
    private var listener: ((WorkerView) -> Unit)? = null
    private var view = WorkerView()
    private var foreground = false

    internal inner class LocalBinder : Binder() {
        fun observe(observer: ((WorkerView) -> Unit)?) {
            listener = observer
            observer?.invoke(view)
            if (observer != null) queue.execute { nextReport = 0 }
        }

        fun start() = launch(null, null, "all")
        fun join(token: String, share: String) = launch(null, token, share)
        fun pair(server: String, share: String) = launch(server, null, share)
        fun stop() = stopWorker()
        fun pause(paused: Boolean) = runCommand(if (paused) "resume" else "pause")
        fun share(share: Boolean) = runCommand("config", "share", if (share) "all" else "none")
        fun boot(boot: Boolean) {
            queue.execute {
                preferences.edit().putBoolean("boot", boot).commit()
                publish()
            }
        }

        fun forget() {
            queue.execute {
                if (wanted || busy) return@execute
                val ok = runCatching { native.workerCommand("reset", "--yes") &&
                    native.workerCommand("config", "unset", "hub_fingerprint") }.getOrDefault(false)
                notice = if (ok) "" else "Could not forget the Hub. Try again."
                report = null
                nextReport = 0
                publish()
            }
        }
    }

    override fun onCreate() {
        super.onCreate()
        native = NativeWorker(this)
        preferences = workerPreferences(this)
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
        if (intent?.action != UI_START) {
            queue.execute {
                if (!stopping.get() && preferences.getBoolean("wanted", false)) {
                    safely { restore() }
                } else {
                    main.post { leaveForeground() }
                }
            }
        }
        return START_STICKY
    }

    private fun launch(server: String?, token: String?, share: String) {
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
                    saveWanted(true)
                    val completed = runCatching { native.provision(server, token, share) }.getOrDefault(false)
                    if (stopping.get()) return@safely
                    report = native.report()
                    val recovered = token != null && report?.optBoolean("paired") == true &&
                        !report?.optString("hub_url").isNullOrBlank()
                    if (!completed && !recovered) {
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
        check(preferences.edit().putBoolean("wanted", value).commit())
        wanted = value
    }

    private fun decline(message: String) {
        wanted = false
        busy = false
        notice = message
        preferences.edit().putBoolean("wanted", false).commit()
        publish()
        main.post { if (foreground) leaveForeground() }
    }

    private fun stopWorker() {
        stopping.set(true)
        native.cancelCommand()
        queue.execute {
            safely {
                if (preferences.edit().putBoolean("wanted", false).commit()) {
                    wanted = false
                    busy = false
                    notice = ""
                    restartAt = 0
                    stopChildren()
                    nextReport = 0
                    publish()
                    main.post { leaveForeground() }
                } else {
                    // Never leave a Stop request waiting on disk I/O.
                    wanted = false
                    busy = false
                    stopChildren()
                    notice = "The Worker stopped. Android could not save the Stop preference."
                    publish()
                    main.post { leaveForeground() }
                }
            }
        }
    }

    private fun stopChildren() {
        try {
            NativeWorker.reap(worker)
        } finally {
            worker = null
            NativeWorker.reap(tmux)
            tmux = null
            expectingTmux = false
        }
    }

    private fun runCommand(vararg args: String) {
        queue.execute {
            if (busy || stopping.get()) return@execute
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
        if (now < restartAt || stopping.get() || !native.tmuxReady()) return
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
        val v = WorkerView(
            wanted, busy, status, notice, r?.optString("hub_url").orEmpty(), workerWords, hubWords,
            r?.optBoolean("paired") == true, r?.optInt("sessions", -1)?.takeIf { it >= 0 }?.toString().orEmpty(),
            share?.optString("mode") != "none", preferences.getBoolean("boot", false),
            livePairing?.optString("code").orEmpty(), if (livePairing != null) expires else "",
        )
        main.post {
            if (v != view) {
                val changed = view.status != v.status
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
            .setContentText(status)
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
    }
}

class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        val preferences = workerPreferences(context)
        if (preferences.getBoolean("boot", false) && preferences.getBoolean("wanted", false)) {
            try {
                context.startForegroundService(Intent(context, WorkerService::class.java))
            } catch (_: IllegalStateException) {
                // A restricted app may have to be opened and started manually.
            }
        }
    }
}
