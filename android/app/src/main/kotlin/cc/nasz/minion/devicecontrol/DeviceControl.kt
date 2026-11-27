// Device control core (design 4.3-4.7): enable/disable, pairing, the single verified adb connection, operations
// for the bridge socket, status and the app-side audit lines. One instance per app process.
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

enum class DcState { UNAVAILABLE, OFF, UNPAIRED, PAIRED, WAITING_WIFI, ENDPOINT_UNVERIFIED, READY }

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
        !paired -> DcState.UNPAIRED
        conn?.closed == false -> DcState.READY
        lastCode == "endpoint_unverified" -> DcState.ENDPOINT_UNVERIFIED
        lastCode == "needs_wifi" || choice() is EndpointChoice.None -> DcState.WAITING_WIFI
        else -> DcState.PAIRED
    }

    /** One line for the app screen and the foreground notification (design 4.7). */
    fun statusLine(): String = when (state) {
        DcState.UNAVAILABLE -> "Device control: not available in this build"
        DcState.OFF -> "Device control: off"
        DcState.UNPAIRED -> "Device control: not paired"
        DcState.PAIRED -> "Device control: paired, not connected"
        DcState.WAITING_WIFI -> "Device control: waiting for Wi-Fi"
        DcState.ENDPOINT_UNVERIFIED -> "Device control: endpoint unverified"
        DcState.READY -> "Device control: on (TLS loopback, port $connPort)"
    }

    // --- Owner actions (app only) --------------------------------------------------------------------------------

    /** Enable on the device. Returns null or the reason it was refused. */
    fun enable(): String? {
        if (!available) return "Device control is not available in this build."
        if (unshared != true) return "Device control needs a dedicated Worker: turn off sharing first (the Worker must be running)."
        prefs.edit().putBoolean(ENABLED, true).apply()
        audit("enable", "ok")
        startDiscovery()
        if (paired) background.execute { runCatching { ensureConnection() } }
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
        if (enabled && paired) background.execute { runCatching { ensureConnection() } }
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
            if (enabled) startDiscovery()
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
                throw BridgeException("needs_wifi", "needs Wi-Fi: the adb connection was lost")
            }
            throw BridgeException("internal", "adb operation failed")
        } finally {
            streams.decrementAndGet()
            changed()
        }
    }

    /**
     * The Worker refuses calls while the state is WAITING_WIFI, so the bridge retries in the background when an
     * endpoint is known again (manual port, or an NSD record without a change event), at most every 5 s. The call
     * that sees WAITING_WIFI is still refused honestly; the next one finds READY.
     */
    private fun retryIfWaiting() {
        if (!enabled || !paired || conn?.closed == false || lastCode != "needs_wifi") return
        if (choice() !is EndpointChoice.Port) return
        val now = SystemClock.elapsedRealtime()
        if (now - lastRetry < 5_000) return
        lastRetry = now
        background.execute { runCatching { ensureConnection() } }
    }

    private fun checkedConnection(): AdbConnection {
        if (!available) throw BridgeException("not_available", "device control is not available in this build")
        if (!enabled) throw BridgeException("disabled", "device control is disabled")
        if (unshared != true) throw BridgeException("shared", "Worker is shared")
        if (!paired) throw BridgeException("not_paired", "not paired: pair in the Minion app")
        return ensureConnection()
    }

    // --- Connection ----------------------------------------------------------------------------------------------

    private fun choice(): EndpointChoice =
        EndpointChooser.choose(manualPort, connectWatcher?.services.orEmpty(), EndpointChooser.ownAddresses())

    private fun ensureConnection(): AdbConnection = synchronized(connectLock) {
        conn?.takeIf { !it.closed }?.let { return it }
        if (!enabled) throw BridgeException("disabled", "device control is disabled")
        val (port, source) = when (val ch = choice()) {
            is EndpointChoice.Port -> ch.port to ch.source
            is EndpointChoice.None -> fail("needs_wifi", "needs Wi-Fi: Wireless debugging is off (no adb endpoint on this device)")
            is EndpointChoice.Ambiguous -> fail("ambiguous", "several adb endpoints on this device: enter the connection port in the Minion app")
        }
        val started = SystemClock.elapsedRealtime()
        try {
            val c = AdbBridge(loadKey()).connect(port)
            if (!enabled) { c.close(); throw BridgeException("disabled", "device control is disabled") }
            conn = c; connPort = port; connSource = source
            lastCode = ""; lastError = ""; lastOk = Instant.now().toString()
            audit("provenance", "ok", started)
            changed()
            return c
        } catch (e: BridgeException) {
            when (e.code) {
                "not_paired" -> { prefs.edit().putBoolean(PAIRED, false).apply(); audit("revoked", "denied:not_paired", started) }
                "endpoint_unverified" -> audit("provenance", "denied:provenance", started)
            }
            fail(e.code, e.message ?: e.code)
        }
    }

    private fun fail(code: String, message: String): Nothing {
        lastCode = code; lastError = message
        changed()
        throw BridgeException(code, message)
    }

    private fun closeConnection() {
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
        if (conn == null && enabled && paired && ch is EndpointChoice.Port) background.execute { runCatching { ensureConnection() } }
        changed()
    }

    // --- Audit (design 4.8): app-side events only, never a code, key, command or output -------------------------

    private fun audit(op: String, result: String, started: Long = SystemClock.elapsedRealtime()) {
        runCatching {
            val dir = File(stateDir, "adb")
            if (!dir.isDirectory) { dir.mkdirs(); android.system.Os.chmod(dir.path, 448) }
            val line = JSONObject()
                .put("time", Instant.now().toString()).put("actor", "local").put("session", "app")
                .put("op", op).put("transport", "tls-loopback").put("result", result)
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

        /** Compiled in, off unless the build flag is on (design 9), and only where the platform exporter exists. */
        val available: Boolean get() = BuildConfig.DEVICE_CONTROL && Build.VERSION.SDK_INT >= 31

        @Volatile private var instance: DeviceControl? = null

        fun get(context: Context): DeviceControl =
            instance ?: synchronized(this) { instance ?: DeviceControl(context).also { instance = it } }
    }
}
