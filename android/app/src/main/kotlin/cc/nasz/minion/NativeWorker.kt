package cc.nasz.minion

import android.content.Context
import android.net.LocalSocket
import android.net.LocalSocketAddress
import android.os.Process
import android.system.Os
import android.system.OsConstants
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
    private val minionFiles = File(home, MINION_FILES)
    private val state = File(root, "state")
    private val terminfo = File(run, "terminfo")
    private val tmuxConfig = File(run, "tmux.conf")
    private val tmuxSocket = File(run, "tmux.sock")
    private val controlSocket = File(run, "minion/worker.sock")
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

    // The folder of every file transfer on this device (files.v1, BRD 5.4).
    val minionFilesPath: String get() = minionFiles.path

    fun startWorker(): java.lang.Process {
        prepare()
        prepareMinionFiles()
        return start("libminion.so", listOf("worker", "run"))
    }

    // The local file transfer switch as the Worker reads it at every request
    // (config.ResolveFileTransfer): the value saved in the config file, on when
    // none is saved, off when the file cannot be read or holds another value.
    // The app removes every MINION_ variable, so no environment value applies.
    fun fileTransferMode(): String = runCatching {
        val file = File(state, CONFIG_FILE)
        if (!file.exists()) return@runCatching FILE_TRANSFER_ON
        check(file.length() <= 1 shl 20)
        val text = file.readText(UTF_8)
        if (text.isBlank()) return@runCatching FILE_TRANSFER_ON
        when (val value = JSONObject(text).opt(FILE_TRANSFER_KEY)) {
            null -> FILE_TRANSFER_ON
            is String -> value.takeIf { it in FILE_TRANSFER_MODES } ?: FILE_TRANSFER_OFF
            else -> FILE_TRANSFER_OFF
        }
    }.getOrDefault(FILE_TRANSFER_OFF)

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
        prepared = true
    }

    // The app-private "Minion files" folder (mode 0700, no storage permission).
    // The Go Worker derives the same path from HOME (agent.AndroidConfine),
    // creates it as well and confines every transfer to it, so nothing else is
    // passed to it. A failure here never stops the Worker: it then reports file
    // transfer as unavailable. A name that is not a directory of the app UID,
    // such as a symlink a shell put there, is left alone for the Worker to refuse.
    private fun prepareMinionFiles() {
        runCatching {
            if (!minionFiles.exists()) Os.mkdir(minionFiles.path, 448) // 0700
            val stat = Os.lstat(minionFiles.path)
            if (OsConstants.S_ISDIR(stat.st_mode) && stat.st_uid == Os.getuid() && (stat.st_mode and 511) != 448) {
                Os.chmod(minionFiles.path, 448)
            }
        }
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

    private fun start(binary: String, args: List<String>, stdout: File? = null): java.lang.Process {
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
        const val MINION_FILES = "Minion files"
        const val FILE_TRANSFER_KEY = "file_transfer"
        const val FILE_TRANSFER_ON = "on"
        const val FILE_TRANSFER_OFF = "off"
        const val FILE_TRANSFER_SEND_ONLY = "send_only"
        const val FILE_TRANSFER_RECEIVE_ONLY = "receive_only"
        private const val CONFIG_FILE = "config.json"
        private val FILE_TRANSFER_MODES = setOf(
            FILE_TRANSFER_ON, FILE_TRANSFER_SEND_ONLY, FILE_TRANSFER_RECEIVE_ONLY, FILE_TRANSFER_OFF,
        )

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
