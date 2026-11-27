package cc.nasz.minion

import android.content.Context
import android.net.LocalSocket
import android.net.LocalSocketAddress
import android.os.Process
import android.system.Os
import cc.nasz.minion.devicecontrol.BridgeToken
import cc.nasz.minion.devicecontrol.DeviceControl
import org.json.JSONObject
import java.io.DataInputStream
import java.io.DataOutputStream
import java.io.File
import java.nio.charset.StandardCharsets.UTF_8
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference

internal data class UsageRegistration(
    val workerId: String,
    val hubFingerprint: String,
    val owner: String,
    val accepted: Boolean,
    val required: Boolean,
)

internal class NativeWorker(private val context: Context) {
    private val root = context.noBackupFilesDir
    private val run = File(root, "r")
    private val home = File(root, "home")
    private val state = File(root, "state")
    private val terminfo = File(run, "terminfo")
    private val tmuxConfig = File(run, "tmux.conf")
    private val tmuxSocket = File(run, "tmux.sock")
    private val controlSocket = File(run, "minion/worker.sock")
    private val bridgeSocket = File(run, "minion/adb-bridge.sock")
    private val shellRc = File(run, "minion.shrc")
    private val nativeDir = File(context.applicationInfo.nativeLibraryDir)
    private val command = AtomicReference<java.lang.Process>()
    private val cancellation = AtomicLong()
    private var prepared = false

    fun startTmux(): java.lang.Process {
        prepare()
        return start("libtmux.so", listOf("-S", tmuxSocket.path, "-f", tmuxConfig.path, "-D"))
    }

    fun tmuxReady(): Boolean = tmuxSocket.exists() &&
        run("libtmux.so", listOf("-N", "-S", tmuxSocket.path, "show-options", "-gqv", "exit-empty"), timeout = 2).success

    // With device control compiled in, every Worker start gets a fresh bridge socket and token. The token goes to
    // the Worker's stdin as one line (never into the environment); the socket path goes into its environment.
    fun startWorker(): java.lang.Process {
        val control = if (DeviceControl.available) DeviceControl.get(context) else null
        val token = control?.startBridge(bridgeSocket)
        val extra = if (token != null) mapOf(BRIDGE_ENVIRONMENT to bridgeSocket.path) else emptyMap()
        val process = try { start("libminion.so", listOf("worker", "run"), extra = extra) } catch (e: Exception) {
            control?.stopBridge()
            throw e
        }
        if (token != null) {
            runCatching { process.outputStream.use { it.write(BridgeToken.stdinLine(token).toByteArray(UTF_8)) } }
        }
        return process
    }

    fun stopBridge() {
        if (DeviceControl.available) DeviceControl.get(context).stopBridge()
    }

    fun provision(server: String?, token: String?, share: String, acceptUsage: Boolean): Boolean {
        val args = mutableListOf("worker", "up", "--configure-only", "--non-interactive-share", share)
        if (acceptUsage) args.add("--accept-usage")
        if (token != null) args.addAll(listOf("--join", "-"))
        if (server != null) args.addAll(listOf("--server", server))
        return run("libminion.so", args, token, timeout = 32).success
    }

    fun workerCommand(vararg args: String): Boolean =
        run("libminion.so", listOf("worker") + args, timeout = 8).success

    fun report(): JSONObject? {
        val result = run("libminion.so", listOf("worker", "status", "--json"), timeout = 4, capture = true)
        return if (result.success && result.output.isNotBlank()) JSONObject(result.output) else null
    }

    fun usageRegistration(): UsageRegistration? {
        val result = run("libminion.so", listOf("worker", "config", "usage", "--json"), timeout = 4, capture = true)
        if (!result.success || result.output.isBlank()) return null
        val json = JSONObject(result.output)
        val workerId = json.optString("worker_id")
        val hubFingerprint = json.optString("hub_fingerprint")
        val owner = json.optString("owner")
        if (!json.optBoolean("enrolled") || workerId.isBlank() || hubFingerprint.isBlank() || owner.isBlank()) return null
        return UsageRegistration(workerId, hubFingerprint, owner, json.optBoolean("accepted"), json.optBoolean("required"))
    }

    fun acceptUsage(registration: UsageRegistration): Boolean = workerCommand(
        "config", "usage", "--accept",
        "--expect-worker-id", registration.workerId,
        "--expect-hub-fingerprint", registration.hubFingerprint,
        "--expect-owner", registration.owner,
    )

    // Only the app UID can connect to this filesystem socket. Keep the private
    // pairing fields in memory and verify the UID and state directory before use.
    fun pairing(): JSONObject? = runCatching {
        LocalSocket().use { socket ->
            socket.soTimeout = 750
            socket.connect(LocalSocketAddress(controlSocket.path, LocalSocketAddress.Namespace.FILESYSTEM))
            check(socket.peerCredentials.uid == Process.myUid())
            val request = """{"type":"op.request","op":"worker_status"}""".toByteArray(UTF_8)
            DataOutputStream(socket.outputStream).apply {
                writeInt(request.size)
                write(request)
                flush()
            }
            val input = DataInputStream(socket.inputStream)
            val size = input.readInt()
            check(size in 1..(1 shl 20))
            val response = ByteArray(size)
            input.readFully(response)
            val frame = JSONObject(String(response, UTF_8))
            val result = frame.optJSONObject("result")
            if (frame.optString("type") != "op.result" || result?.optString("state_dir") != state.path) {
                null
            } else {
                result.optJSONObject("pairing")
            }
        }
    }.getOrNull()

    fun cancelCommand() {
        cancellation.incrementAndGet()
        command.get()?.destroy()
    }

    private fun prepare() {
        if (prepared) return
        for (directory in listOf(root, run, home, state)) {
            check(directory.isDirectory || directory.mkdirs())
            Os.chmod(directory.path, 448) // 0700
        }
        check(controlSocket.path.toByteArray(UTF_8).size <= 100)
        copyAssets("terminfo", terminfo)
        copyAssets("tmux.conf", tmuxConfig)
        // Interactive shells of the panes get `minion` as an alias of the packaged binary (design 4.6). The
        // owner's own ~/.mkshrc still runs, because ENV replaces it.
        shellRc.writeText(
            "alias minion='\"\$MINION_BINARY\"'\n" +
                "[ -r \"\$HOME/.mkshrc\" ] && . \"\$HOME/.mkshrc\"\n", UTF_8,
        )
        prepared = true
    }

    private fun copyAssets(path: String, dest: File) {
        val contents = context.assets.list(path).orEmpty()
        if (contents.isNotEmpty()) {
            check(dest.isDirectory || dest.mkdirs())
            for (entry in contents) copyAssets("$path/$entry", File(dest, entry))
        } else {
            val temp = File.createTempFile("asset", ".tmp", dest.parentFile)
            try {
                context.assets.open(path).use { input -> temp.outputStream().use { input.copyTo(it) } }
                check(temp.renameTo(dest))
            } finally {
                temp.delete()
            }
        }
    }

    private fun start(
        binary: String, args: List<String>, stdout: File? = null, extra: Map<String, String> = emptyMap(),
    ): java.lang.Process {
        prepare()
        val executable = File(nativeDir, binary)
        val launcher = File(nativeDir, "libminion-launch.so")
        val nullFile = File("/dev/null")
        val builder = ProcessBuilder(listOf(launcher.path, Process.myPid().toString(), executable.path) + args)
            .directory(home)
            .redirectOutput(stdout ?: nullFile)
            .redirectError(nullFile)
        builder.environment().apply {
            keys.removeAll { it.startsWith("MINION_") || it.startsWith("XDG_") || it.startsWith("LC_") }
            remove("TMUX")
            remove("TMUX_PANE")
            put("HOME", home.path)
            put("TMPDIR", run.path)
            put("TMUX_TMPDIR", run.path)
            put("XDG_RUNTIME_DIR", run.path)
            put("TERMINFO", terminfo.path)
            put("TERM", "xterm-256color")
            put("LANG", "C.UTF-8")
            put("SHELL", "/system/bin/sh")
            put("PATH", "/system/bin:/system/xbin:/sbin:/debug_ramdisk:/su/bin:/data/adb/ksu/bin:/vendor/bin")
            put("MINION_STATE_DIR", state.path)
            put("MINION_MANAGED_HOST", "android")
            put("MINION_TMUX_BINARY", File(nativeDir, "libtmux.so").path)
            put("MINION_TMUX_SOCKET", tmuxSocket.path)
            put("MINION_TMUX_CONFIG", tmuxConfig.path)
            // nativeLibraryDir changes on every APK update: refreshed on every start, inherited by the panes.
            put("MINION_BINARY", File(nativeDir, "libminion.so").path)
            put("ENV", shellRc.path)
            putAll(extra)
            // Read on every start, so it survives Worker, app and device restarts.
            HubDialOverride.saved(context).takeIf { it.isNotBlank() }?.let { put(HubDialOverride.ENVIRONMENT, it) }
        }
        return builder.start()
    }

    private data class Result(val success: Boolean, val output: String = "")

    private fun run(
        binary: String,
        args: List<String>,
        token: String? = null,
        timeout: Long,
        capture: Boolean = false,
    ): Result {
        prepare()
        val generation = cancellation.get()
        val stdout = if (capture) File.createTempFile("status", ".json", run) else null
        try {
            val process = start(binary, args, stdout)
            command.set(process)
            try {
                if (cancellation.get() != generation) return Result(false)
                process.outputStream.use { output ->
                    if (token != null) output.write((token + "\n").toByteArray(UTF_8))
                }
                if (!process.waitFor(timeout, TimeUnit.SECONDS) || process.exitValue() != 0) return Result(false)
                if (stdout != null && stdout.length() <= 256 * 1024) return Result(true, stdout.readText(UTF_8))
                return Result(true)
            } finally {
                command.compareAndSet(process, null)
                reap(process)
            }
        } finally {
            stdout?.delete()
        }
    }

    companion object {
        const val BRIDGE_ENVIRONMENT = "MINION_ADB_BRIDGE_SOCKET"

        fun reap(process: java.lang.Process?) {
            if (process == null) return
            if (process.isAlive) process.destroy()
            if (!process.waitFor(3, TimeUnit.SECONDS)) {
                process.destroyForcibly()
                check(process.waitFor(3, TimeUnit.SECONDS)) { "A supervised child did not exit" }
            }
        }
    }
}
