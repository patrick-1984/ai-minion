// Device control: the bridge socket server (Worker -> app). Filesystem Unix socket, 0600, peer uid == app uid,
// per-start token, one operation per connection. No TCP listener anywhere.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import android.net.LocalServerSocket
import android.net.LocalSocket
import android.net.LocalSocketAddress
import android.os.Process
import android.system.Os
import android.system.OsConstants
import org.json.JSONObject
import java.io.DataInputStream
import java.io.DataOutputStream
import java.io.File
import java.io.IOException
import java.util.concurrent.Semaphore
import java.util.concurrent.atomic.AtomicBoolean

/** What the server asks of the device-control core. */
interface BridgeHandler {
    fun status(): JSONObject
    fun disable(reason: String)
    /** Runs shell/screencap/input; returns the exit code or throws [BridgeException]. */
    fun run(op: String, request: JSONObject, sink: OutputSink): Int
}

class BridgeServer(private val socketFile: File, private val token: String, private val handler: BridgeHandler) {
    private val stopped = AtomicBoolean()
    private val slots = Semaphore(OpLimits.MAX_CONCURRENT)
    private val clients = java.util.concurrent.ConcurrentHashMap.newKeySet<LocalSocket>()
    private var bound: LocalSocket? = null
    private var server: LocalServerSocket? = null

    fun start() {
        val dir = socketFile.parentFile!!
        check(dir.isDirectory || dir.mkdirs())
        Os.chmod(dir.path, 448) // 0700
        socketFile.delete()
        check(socketFile.path.toByteArray(Charsets.UTF_8).size <= 100) { "bridge socket path too long" }
        val s = LocalSocket(LocalSocket.SOCKET_STREAM)
        s.bind(LocalSocketAddress(socketFile.path, LocalSocketAddress.Namespace.FILESYSTEM))
        Os.chmod(socketFile.path, 384) // 0600
        bound = s
        server = LocalServerSocket(s.fileDescriptor)
        Thread({ acceptLoop() }, "adb-bridge-accept").apply { isDaemon = true }.start()
    }

    fun stop() {
        if (!stopped.compareAndSet(false, true)) return
        // LocalServerSocket.close() alone does not wake accept(): shut the descriptor down first.
        runCatching { bound?.fileDescriptor?.let { Os.shutdown(it, OsConstants.SHUT_RDWR) } }
        runCatching { server?.close() }
        runCatching { bound?.close() }
        for (c in clients) runCatching { c.close() }
        socketFile.delete()
    }

    private fun acceptLoop() {
        while (!stopped.get()) {
            val client = try { server?.accept() ?: return } catch (_: IOException) { if (stopped.get()) return else continue }
            if (stopped.get()) { runCatching { client.close() }; return }
            Thread({ serve(client) }, "adb-bridge-client").apply { isDaemon = true }.start()
        }
    }

    private fun serve(client: LocalSocket) {
        clients.add(client)
        try {
            client.use { c ->
                if (c.peerCredentials.uid != Process.myUid()) return
                val out = DataOutputStream(c.outputStream)
                val inp = DataInputStream(c.inputStream)
                c.soTimeout = 5_000
                val (channel, payload) = try { BridgeFrames.read(inp, 256 * 1024) } catch (_: IOException) { return }
                c.soTimeout = 0
                val request = runCatching { JSONObject(String(payload, Charsets.UTF_8)) }.getOrNull()
                if (channel != BridgeFrames.CONTROL || request == null) return error(out, "bad_request", "expected one JSON request")
                if (!BridgeToken.matches(token, request.optString("token", ""))) return error(out, "bad_token", "bridge token mismatch")
                when (val op = request.optString("op")) {
                    "status" -> control(out, handler.status().put("type", "status"))
                    "disable" -> {
                        handler.disable(request.optString("reason", "worker"))
                        control(out, JSONObject().put("type", "ok"))
                    }
                    "shell", "screencap", "input" -> runOp(op, request, out)
                    else -> error(out, "bad_request", "unknown op")
                }
            }
        } catch (_: Exception) {
            // The caller sees the closed socket; nothing about the request is logged.
        } finally {
            clients.remove(client)
        }
    }

    private fun runOp(op: String, request: JSONObject, out: DataOutputStream) {
        if (!slots.tryAcquire()) return error(out, "busy", "too many device-control operations at once")
        try {
            var started = false
            val sink = OutputSink { channel, data ->
                if (!started) { control(out, JSONObject().put("type", "started")); started = true }
                BridgeFrames.writeData(out, channel, data)
            }
            val code = try { handler.run(op, request, sink) } catch (e: BridgeException) {
                return error(out, e.code, e.message ?: e.code)
            }
            if (!started) control(out, JSONObject().put("type", "started"))
            control(out, JSONObject().put("type", "exit").put("code", code))
        } finally {
            slots.release()
        }
    }

    private fun control(out: DataOutputStream, json: JSONObject) =
        BridgeFrames.write(out, BridgeFrames.CONTROL, json.toString().toByteArray(Charsets.UTF_8))

    private fun error(out: DataOutputStream, code: String, message: String) {
        runCatching { control(out, JSONObject().put("type", "error").put("code", code).put("message", message)) }
    }
}
