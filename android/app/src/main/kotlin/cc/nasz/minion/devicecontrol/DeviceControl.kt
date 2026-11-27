// Device control core (design 4.3-4.7): enable/disable, pairing, the single verified adb connection (legacy adb on
// 127.0.0.1:5555 first, then Wireless debugging TLS), operations for the bridge socket, status and the app-side audit
// lines. One instance per app process.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import android.content.Context
import android.os.Build
import android.os.SystemClock
import cc.nasz.minion.BuildConfig
import org.json.JSONObject
import java.io.File
import java.io.FileOutputStream
import java.io.IOException
import java.time.Instant
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicInteger

enum class DcState { UNAVAILABLE, OFF, UNPAIRED, PAIRED, AWAITING_AUTHORIZATION, WAITING_WIFI, ENDPOINT_UNVERIFIED, READY }

class DeviceControl private constructor(context: Context) : BridgeHandler {
    private val app = context.applicationContext
    private val prefs = app.getSharedPreferences("device_control", Context.MODE_PRIVATE)
    private val stateDir = File(app.noBackupFilesDir, "state")
    private val connectLock = Any()
    private val background = Executors.newSingleThreadExecutor { r -> Thread(r, "device-control").apply { isDaemon = true } }
    private val streams = AtomicInteger()
    @Volatile private var conn: AdbConnection? = null
    @Volatile private var connPort = 0
    @Volatile private var connSource = ""
    @Volatile private var connTransport = ""
    /** The legacy handshake waiting for "Allow debugging?" on the TV, null otherwise. */
    @Volatile private var pendingAuth: LegacyAttempt.AwaitingUser? = null
    /** Something speaks legacy adb on 127.0.0.1:5555 (last attempt); then no pairing is needed. */
    @Volatile private var legacySeen = false
    @Volatile private var lastCode = ""
    @Volatile private var lastError = ""
    @Volatile private var lastOk = ""
    @Volatile private var lastRetry = 0L
    @Volatile private var keyStorage = ""
    @Volatile private var key: AdbKey? = null
    @Volatile private var server: BridgeServer? = null
    private var connectWatcher: NsdWatcher? = null
    private var pairingWatcher: NsdWatcher? = null
    private val listeners = java.util.concurrent.CopyOnWriteArraySet<() -> Unit>()

    /** Share mode of the Worker as last reported (true = "none"), null while unknown. Set by WorkerService. */
    @Volatile var unshared: Boolean? = null
        private set

    val enabled: Boolean get() = available && prefs.getBoolean(ENABLED, false)
    val paired: Boolean get() = prefs.getBoolean(PAIRED, false)
    val manualPort: Int? get() = prefs.getInt(MANUAL_PORT, 0).takeIf { EndpointChooser.validPort(it) }

    fun observe(listener: () -> Unit) { listeners.add(listener) }
    fun unobserve(listener: () -> Unit) { listeners.remove(listener) }
    private fun changed() { for (l in listeners) runCatching { l() } }

    val state: DcState get() = when {
        !available -> DcState.UNAVAILABLE
        !enabled -> DcState.OFF
        conn?.closed == false -> DcState.READY
        pendingAuth != null -> DcState.AWAITING_AUTHORIZATION
        lastCode == "endpoint_unverified" -> DcState.ENDPOINT_UNVERIFIED
        legacySeen -> DcState.PAIRED // connects on demand; asks on the TV again if it was not allowed
        !paired -> DcState.UNPAIRED
        lastCode == "needs_wifi" || choice() is EndpointChoice.None -> DcState.WAITING_WIFI
        else -> DcState.PAIRED
    }

    /** One line for the app screen and the foreground notification (design 4.7). */
    fun statusLine(): String = when (state) {
        DcState.UNAVAILABLE -> "Device control: not available in this build"
        DcState.OFF -> "Device control: off"
        DcState.UNPAIRED -> "Device control: no adb on 127.0.0.1:${LegacyConnector.PORT}, Wireless debugging not paired"
        DcState.PAIRED -> when {
            lastCode == LegacyConnector.NEEDS_AUTH -> "Device control: not allowed on the TV yet (it asks again on the next use)"
            legacySeen -> "Device control: not connected (legacy adb on 127.0.0.1:${LegacyConnector.PORT})"
            else -> "Device control: paired, not connected"
        }
        DcState.AWAITING_AUTHORIZATION -> "Device control: first connection. ${LegacyConnector.CONFIRM}"
        DcState.WAITING_WIFI -> "Device control: waiting for Wi-Fi"
        DcState.ENDPOINT_UNVERIFIED -> "Device control: endpoint unverified"
        DcState.READY -> if (connTransport == TRANSPORT_LEGACY) "Device control: on (legacy adb, 127.0.0.1:$connPort)"
            else "Device control: on (TLS loopback, port $connPort)"
    }

    // --- Owner actions (app only) --------------------------------------------------------------------------------

    /** Enable on the device. Returns null or the reason it was refused. */
    fun enable(): String? {
        if (!available) return "Device control is not available in this build."
        if (unshared != true) return "Device control needs a dedicated Worker: turn off sharing first (the Worker must be running)."
        prefs.edit().putBoolean(ENABLED, true).apply()
        audit("enable", "ok")
        startDiscovery()
        // Also unpaired: legacy adb on 127.0.0.1:5555 needs no pairing; a first connection shows the TV dialog now.
        background.execute { runCatching { ensureConnection() } }
        changed()
        return null
    }

    /** Disable from anywhere (app, Hub via the Worker, share change). Closes every stream at once. */
    override fun disable(reason: String) {
        val was = enabled
        prefs.edit().putBoolean(ENABLED, false).apply()
        closeConnection()
        stopDiscovery()
        stopPairing()
        if (was) audit("disable", "ok")
        changed()
    }

    fun setManualPort(port: Int?) {
        prefs.edit().putInt(MANUAL_PORT, port ?: 0).apply()
        closeConnection()
        lastCode = ""
        if (enabled) background.execute { runCatching { ensureConnection() } }
        changed()
    }

    /** "Forget" guidance: Android's own list is the place to revoke (design 5). */
    fun forgetGuidance(): String =
        "Device control is off in Minion. To remove the authorization from Android too: Settings > Developer options > " +
            "Wireless debugging > Paired devices > ${AdbKeys.NAME} > Forget."

    /** Pairing port hints from NSD on this device's own addresses (prefills the keypad). */
    fun pairingPortHint(): Int? =
        (EndpointChooser.choose(null, pairingWatcher?.services.orEmpty(), EndpointChooser.ownAddresses()) as? EndpointChoice.Port)?.port

    fun startPairing() {
        if (!enabled) return
        synchronized(this) {
            if (pairingWatcher == null) pairingWatcher = NsdWatcher(app, NsdWatcher.PAIRING) { changed() }.also { it.start() }
        }
    }

    fun stopPairing() {
        synchronized(this) { pairingWatcher?.stop(); pairingWatcher = null }
    }

    /** Pairs in this process; [code] is wiped. [done] gets null on success or the reason, on a background thread. */
    fun pair(port: Int, code: CharArray, done: (String?) -> Unit) {
        val copy = code.copyOf()
        code.fill('\u0000')
        background.execute {
            val started = SystemClock.elapsedRealtime()
            val result = try {
                if (!enabled) throw BridgeException("disabled", "Device control is off.")
                AdbBridge(loadKey()).pair(port, copy)
                prefs.edit().putBoolean(PAIRED, true).apply()
                lastCode = ""; lastError = ""
                audit("pair", "ok", started)
                null
            } catch (e: BridgeException) {
                audit("pair", "error:${e.code}", started)
                e.message ?: e.code
            } catch (_: Exception) {
                audit("pair", "error:internal", started)
                AdbBridge.CODE_REJECTED
            } finally {
                copy.fill('\u0000')
            }
            stopPairing()
            changed()
            done(result)
            if (result == null) runCatching { ensureConnection() }
            changed()
        }
    }

    // --- Bridge socket lifecycle (WorkerService) -----------------------------------------------------------------

    /** Starts the bridge socket for one Worker start; returns the token for the Worker's stdin, or null. */
    fun startBridge(socketFile: File): String? {
        if (!available) return null
        stopBridge()
        val token = BridgeToken.generate()
        return runCatching {
            BridgeServer(socketFile, token, this).also { it.start(); server = it }
            if (enabled) {
                startDiscovery()
                if (conn?.closed != false) background.execute { runCatching { ensureConnection() } }
            }
            token
        }.getOrNull()
    }

    fun stopBridge() {
        server?.stop()
        server = null
    }

    /** Called with every Worker report. Sharing turned on while enabled disables device control (fail closed). */
    fun observeShare(shareMode: String?) {
        unshared = when (shareMode) { null, "" -> null; "none" -> true; else -> false }
        if (enabled && unshared == false) disable("share")
    }

    // --- BridgeHandler -------------------------------------------------------------------------------------------

    override fun status(): JSONObject = JSONObject()
        .also { retryIfWaiting() }
        .put("type", "status")
        .put("feature_enabled", available)
        .put("enabled", enabled)
        .put("state", state.name)
        .put("port", if (conn?.closed == false) connPort else 0)
        .put("port_source", if (conn?.closed == false) connSource else "")
        .put("transport", if (conn?.closed == false) connTransport else if (pendingAuth != null) TRANSPORT_LEGACY else "")
        .put("key_storage", keyStorage)
        .put("key_warning", if (keyStorage == "app-file") AdbKeys.FILE_WARNING else "")
        .put("active_streams", streams.get())
        .put("last_ok", lastOk)
        .put("last_error", lastCode) // the class (an error code), never output
        .put("last_error_message", lastError)
        .put("provenance_denied", ProvenanceRegistry.shared.denied.get())

    override fun run(op: String, request: JSONObject, sink: OutputSink): Int {
        // The Worker sends `command` (built and quoted by it); `argv`/`args` are accepted too. Input is parsed back
        // and re-validated here, so the bridge never runs anything but the four `input` forms.
        val command = request.optString("command", "")
        val commandLine = when (op) {
            "shell" -> {
                val argv = request.optJSONArray("argv")
                val list = if (argv != null) (0 until argv.length()).map { argv.optString(it) } else listOf(command)
                try { ShellQuote.commandLine(list) } catch (_: IllegalArgumentException) {
                    throw BridgeException("bad_request", "empty command or NUL in an argument")
                }.also { if (it.isBlank()) throw BridgeException("bad_request", "empty command") }
            }
            "input" -> {
                val args = request.optJSONArray("args")?.let { a -> (0 until a.length()).map { a.optString(it) } }
                    ?: ShellWords.split(command)?.takeIf { it.firstOrNull() == "input" }?.drop(1)
                    ?: throw BridgeException("bad_request", "invalid input arguments")
                InputArgs.commandLine(args) ?: throw BridgeException("bad_request", "invalid input arguments")
            }
            else -> ""
        }
        if (commandLine.length > OpLimits.MAX_COMMAND) throw BridgeException("bad_request", "command too long")
        val c = checkedConnection()
        streams.incrementAndGet()
        changed()
        try {
            val code = if (op == "screencap") {
                val t = request.optLong("timeout_ms", 0)
                AdbOps.screencap(c, if (t > 0) minOf(t, OpLimits.MAX_TIMEOUT_MS) else OpLimits.SCREENCAP_TIMEOUT_MS, sink); 0
            } else {
                AdbOps.shell(c, commandLine, OpLimits.timeout(request.optLong("timeout_ms", 0)), sink)
            }
            lastOk = Instant.now().toString()
            return code
        } catch (e: AdbTimeoutException) {
            throw BridgeException("timeout", "timeout")
        } catch (e: IOException) {
            if (e is BridgeException) throw e
            if (!enabled) throw BridgeException("disabled", "device control disabled")
            if (c.closed) {
                if (conn === c) conn = null
                if (connTransport == TRANSPORT_LEGACY) throw BridgeException("internal", "the adb connection was lost; the next call connects again")
                throw BridgeException("needs_wifi", "needs Wi-Fi: the adb connection was lost")
            }
            throw BridgeException("internal", "adb operation failed")
        } finally {
            streams.decrementAndGet()
            changed()
        }
    }

    /**
     * The Worker refuses calls while the state is WAITING_WIFI or UNPAIRED, so the bridge retries in the background
     * (legacy adb on 127.0.0.1:5555 may be up again after a boot, a TLS endpoint may be known again), at most every
     * 5 s. The call that sees the old state is still refused honestly; the next one finds READY. Never after
     * needs_authorization: that would raise the TV dialog on every status poll.
     */
    private fun retryIfWaiting() {
        if (!enabled || conn?.closed == false || pendingAuth != null) return
        val waiting = lastCode == "needs_wifi" || lastCode == "not_paired" || (lastCode == "" && !legacySeen)
        if (!waiting) return
        val now = SystemClock.elapsedRealtime()
        if (now - lastRetry < 5_000) return
        lastRetry = now
        background.execute { runCatching { ensureConnection() } }
    }

    private fun checkedConnection(): AdbConnection {
        if (!available) throw BridgeException("not_available", "device control is not available in this build")
        if (!enabled) throw BridgeException("disabled", "device control is disabled")
        if (unshared != true) throw BridgeException("shared", "Worker is shared")
        return ensureConnection()
    }

    // --- Connection ----------------------------------------------------------------------------------------------

    private fun choice(): EndpointChoice =
        EndpointChooser.choose(manualPort, connectWatcher?.services.orEmpty(), EndpointChooser.ownAddresses())

    /**
     * The single verified connection: legacy adb on 127.0.0.1:5555 first (no pairing, the TV asks once), then the
     * Wireless debugging TLS port (paired). Both pass the provenance check before any operation is sent.
     */
    private fun ensureConnection(): AdbConnection {
        conn?.takeIf { !it.closed }?.let { return it }
        if (pendingAuth != null) throw BridgeException(LegacyConnector.NEEDS_AUTH, LegacyConnector.CONFIRM)
        return synchronized(connectLock) { connectLocked() }
    }

    private fun connectLocked(): AdbConnection {
        conn?.takeIf { !it.closed }?.let { return it }
        if (pendingAuth != null) throw BridgeException(LegacyConnector.NEEDS_AUTH, LegacyConnector.CONFIRM)
        if (!enabled) throw BridgeException("disabled", "device control is disabled")
        val bridge = AdbBridge(loadKey())
        var legacyError: BridgeException? = null
        val legacyStarted = SystemClock.elapsedRealtime()
        try {
            when (val a = bridge.legacy().attempt()) {
                is LegacyAttempt.Ready -> { legacySeen = true; return adopt(a.conn, LegacyConnector.PORT, "legacy", TRANSPORT_LEGACY, legacyStarted) }
                is LegacyAttempt.AwaitingUser -> {
                    legacySeen = true
                    awaitOwner(a)
                    throw BridgeException(LegacyConnector.NEEDS_AUTH, LegacyConnector.CONFIRM)
                }
                LegacyAttempt.Absent -> legacySeen = false
            }
        } catch (e: BridgeException) {
            if (e.code == LegacyConnector.NEEDS_AUTH) throw e
            legacySeen = true
            if (e.code == "endpoint_unverified") audit("provenance", "denied:provenance", legacyStarted, TRANSPORT_LEGACY)
            legacyError = e
        }
        // Wireless debugging (TLS). A failed legacy endpoint is the more important news when TLS is not there.
        if (!paired) {
            legacyError?.let { fail(it.code, it.message ?: it.code) }
            fail("not_paired", "No adb on 127.0.0.1:${LegacyConnector.PORT} and Wireless debugging is not paired: pair in the Minion app.")
        }
        val (port, source) = when (val ch = choice()) {
            is EndpointChoice.Port -> ch.port to ch.source
            is EndpointChoice.None -> {
                legacyError?.let { fail(it.code, it.message ?: it.code) }
                fail("needs_wifi", "needs Wi-Fi: Wireless debugging is off (no adb endpoint on this device)")
            }
            is EndpointChoice.Ambiguous -> fail("ambiguous", "several adb endpoints on this device: enter the connection port in the Minion app")
        }
        val started = SystemClock.elapsedRealtime()
        try {
            val c = bridge.connect(port)
            return adopt(c, port, source, TRANSPORT_TLS, started)
        } catch (e: BridgeException) {
            when (e.code) {
                "not_paired" -> { prefs.edit().putBoolean(PAIRED, false).apply(); audit("revoked", "denied:not_paired", started) }
                "endpoint_unverified" -> audit("provenance", "denied:provenance", started)
            }
            if (e.code == "needs_wifi" && legacyError != null) fail(legacyError.code, legacyError.message ?: legacyError.code)
            fail(e.code, e.message ?: e.code)
        }
    }

    private fun adopt(c: AdbConnection, port: Int, source: String, transport: String, started: Long): AdbConnection {
        if (!enabled) { c.close(); throw BridgeException("disabled", "device control is disabled") }
        conn = c; connPort = port; connSource = source; connTransport = transport
        lastCode = ""; lastError = ""; lastOk = Instant.now().toString()
        audit("provenance", "ok", started, transport)
        changed()
        return c
    }

    /** The TV shows "Allow debugging?": wait for the owner on an own thread; calls meanwhile get needs_authorization. */
    private fun awaitOwner(a: LegacyAttempt.AwaitingUser) {
        pendingAuth = a
        lastCode = LegacyConnector.NEEDS_AUTH; lastError = LegacyConnector.CONFIRM
        audit("authorize", "pending", transport = TRANSPORT_LEGACY)
        changed()
        Thread({
            val started = SystemClock.elapsedRealtime()
            try {
                val c = a.finish(AUTH_WAIT_MS)
                synchronized(connectLock) {
                    if (pendingAuth !== a) { c.close(); return@synchronized }
                    pendingAuth = null
                    audit("authorize", "ok", started, TRANSPORT_LEGACY)
                    runCatching { adopt(c, LegacyConnector.PORT, "legacy", TRANSPORT_LEGACY, started) }
                }
            } catch (e: BridgeException) {
                if (pendingAuth === a) {
                    lastCode = e.code; lastError = e.message ?: e.code
                    audit(if (e.code == LegacyConnector.NEEDS_AUTH) "authorize" else "provenance",
                        if (e.code == LegacyConnector.NEEDS_AUTH) "denied:not_allowed" else "denied:provenance", started, TRANSPORT_LEGACY)
                }
            } catch (_: Exception) {
                if (pendingAuth === a) { lastCode = "internal"; lastError = "the legacy adb handshake failed" }
            } finally {
                if (pendingAuth === a) pendingAuth = null
                changed()
            }
        }, "adb-legacy-auth").apply { isDaemon = true }.start()
    }

    private fun fail(code: String, message: String): Nothing {
        lastCode = code; lastError = message
        changed()
        throw BridgeException(code, message)
    }

    private fun closeConnection() {
        pendingAuth?.let { pendingAuth = null; it.cancel() }
        val c = conn
        conn = null
        c?.close()
    }

    private fun loadKey(): AdbKey = key ?: AdbKeys.load(stateDir).also { key = it; keyStorage = it.storageDescription() }

    private fun startDiscovery() {
        synchronized(this) {
            if (connectWatcher != null) return
            connectWatcher = NsdWatcher(app, NsdWatcher.CONNECT) { onEndpointsChanged() }.also { it.start() }
        }
    }

    private fun stopDiscovery() {
        synchronized(this) { connectWatcher?.stop(); connectWatcher = null }
    }

    /** NSD hint changed: drop a connection whose port is gone, try one when an endpoint appears. */
    private fun onEndpointsChanged() {
        val c = conn
        val ch = choice()
        if (c != null && connSource == "nsd" && (ch !is EndpointChoice.Port || ch.port != connPort)) {
            closeConnection()
            lastCode = "needs_wifi"; lastError = "the adb endpoint went away (Wi-Fi or Wireless debugging off)"
        }
        if (conn == null && pendingAuth == null && enabled && paired && ch is EndpointChoice.Port) background.execute { runCatching { ensureConnection() } }
        changed()
    }

    // --- Audit (design 4.8): app-side events only, never a code, key, command or output -------------------------

    private fun audit(op: String, result: String, started: Long = SystemClock.elapsedRealtime(), transport: String = TRANSPORT_TLS) {
        runCatching {
            val dir = File(stateDir, "adb")
            if (!dir.isDirectory) { dir.mkdirs(); android.system.Os.chmod(dir.path, 448) }
            val line = JSONObject()
                .put("time", Instant.now().toString()).put("actor", "local").put("session", "app")
                .put("op", op).put("transport", transport).put("result", result)
                .put("duration_ms", SystemClock.elapsedRealtime() - started)
                .toString() + "\n"
            val f = File(dir, "audit.log")
            FileOutputStream(f, true).use { it.write(line.toByteArray(Charsets.UTF_8)) }
            android.system.Os.chmod(f.path, 384)
        }
    }

    companion object {
        private const val ENABLED = "enabled"
        private const val PAIRED = "paired"
        private const val MANUAL_PORT = "manual_port"
        const val TRANSPORT_LEGACY = "legacy-loopback"
        const val TRANSPORT_TLS = "tls-loopback"
        /** How long the TV dialog may wait for the owner. */
        const val AUTH_WAIT_MS = 120_000L

        /** Compiled in, off unless the build flag is on (design 9), and only where the platform exporter exists. */
        val available: Boolean get() = BuildConfig.DEVICE_CONTROL && Build.VERSION.SDK_INT >= 31

        @Volatile private var instance: DeviceControl? = null

        fun get(context: Context): DeviceControl =
            instance ?: synchronized(this) { instance ?: DeviceControl(context).also { instance = it } }
    }
}
