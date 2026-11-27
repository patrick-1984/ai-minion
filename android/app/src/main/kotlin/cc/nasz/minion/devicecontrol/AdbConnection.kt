// Device control: one adb transport (after STLS + TLS) with multiplexed streams. Pure JVM; unit-tested against a
// fake adbd over a socket pair.
// SPDX-License-Identifier: Apache-2.0
//
// Written for Minion from AOSP packages/modules/adb protocol.txt (Apache-2.0). Only OPEN of the services the bridge
// itself names is possible (shell,v2 and exec:screencap); there is no generic "open any service" entry.
package cc.nasz.minion.devicecontrol

import java.io.Closeable
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CountDownLatch
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.Semaphore
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

class AdbTimeoutException(message: String) : IOException(message)

/**
 * A connected adb transport. [input]/[output] are the TLS streams after the device's CNXN was read; [onClose] closes
 * the socket. The reader thread dispatches messages to streams; [close] ends every stream at once (close-all).
 */
class AdbConnection(
    private val input: InputStream,
    private val output: OutputStream,
    private val onClose: () -> Unit,
) : Closeable {
    private val streams = ConcurrentHashMap<Int, AdbStream>()
    private val nextId = AtomicInteger(1)
    private val writeLock = Any()
    @Volatile var closed = false
        private set
    @Volatile var failure: IOException? = null
        private set
    private val reader = Thread({ readLoop() }, "adb-reader").apply { isDaemon = true }

    fun start(): AdbConnection = apply { reader.start() }

    val activeStreams: Int get() = streams.size

    /** Opens one of the bridge's fixed services. Throws if adbd refuses it (CLSE) or does not answer in time. */
    fun open(service: String, timeoutMs: Long = 5_000): AdbStream {
        if (closed) throw IOException("adb connection closed")
        val id = nextId.getAndIncrement()
        val stream = AdbStream(this, id)
        streams[id] = stream
        send(AdbMessage(AdbMessage.A_OPEN, id, 0, (service + "\u0000").toByteArray(Charsets.UTF_8)))
        if (!stream.opened.await(timeoutMs, TimeUnit.MILLISECONDS)) {
            streams.remove(id)
            throw AdbTimeoutException("adb: no answer to OPEN")
        }
        if (stream.remoteId == 0) {
            streams.remove(id)
            throw IOException(failure?.message ?: "adb: service refused")
        }
        return stream
    }

    internal fun send(m: AdbMessage) {
        synchronized(writeLock) {
            if (closed) throw IOException("adb connection closed")
            try {
                output.write(m.encode()); output.flush()
            } catch (e: IOException) {
                fail(e); throw e
            }
        }
    }

    internal fun forget(id: Int) { streams.remove(id) }

    private fun readLoop() {
        try {
            while (!closed) {
                val m = AdbMessage.read(input)
                val s = streams[m.arg1]
                when (m.command) {
                    AdbMessage.A_OKAY -> if (s != null) {
                        if (s.remoteId == 0) { s.remoteId = m.arg0; s.opened.countDown() } else s.writable.release()
                    }
                    AdbMessage.A_WRTE -> if (s != null) {
                        s.inbound.put(m.data)
                        send(AdbMessage(AdbMessage.A_OKAY, s.localId, m.arg0))
                    }
                    AdbMessage.A_CLSE -> if (s != null) {
                        streams.remove(s.localId)
                        s.remoteEnded()
                    }
                    else -> {} // CNXN refresh, AUTH, unknown: ignored after the handshake
                }
            }
        } catch (e: IOException) {
            fail(e)
        }
    }

    private fun fail(e: IOException) {
        if (failure == null) failure = e
        close()
    }

    override fun close() {
        if (closed) return
        closed = true
        runCatching { onClose() }
        for (s in streams.values) s.remoteEnded()
        streams.clear()
    }
}

/** One adb stream. Reads return chunks in order; null = the remote side closed it. */
class AdbStream internal constructor(private val conn: AdbConnection, val localId: Int) : Closeable {
    @Volatile var remoteId = 0
    internal val opened = CountDownLatch(1)
    internal val writable = Semaphore(0)
    internal val inbound = LinkedBlockingQueue<ByteArray>()
    @Volatile private var ended = false

    internal fun remoteEnded() {
        ended = true
        opened.countDown()
        inbound.offer(EOF)
        writable.release(1000)
    }

    /** Next chunk, null at end of stream; throws [AdbTimeoutException] after [timeoutMs]. */
    fun read(timeoutMs: Long): ByteArray? {
        val chunk = inbound.poll(timeoutMs.coerceAtLeast(1), TimeUnit.MILLISECONDS)
            ?: throw AdbTimeoutException("timeout")
        if (chunk === EOF) { inbound.offer(EOF); return null }
        return chunk
    }

    fun write(data: ByteArray, timeoutMs: Long = 5_000) {
        if (ended) throw IOException("adb stream closed")
        conn.send(AdbMessage(AdbMessage.A_WRTE, localId, remoteId, data))
        if (!writable.tryAcquire(timeoutMs, TimeUnit.MILLISECONDS)) throw AdbTimeoutException("adb: write not acknowledged")
        if (ended) throw IOException("adb stream closed")
    }

    override fun close() {
        conn.forget(localId)
        if (!ended && remoteId != 0) runCatching { conn.send(AdbMessage(AdbMessage.A_CLSE, localId, remoteId)) }
        remoteEnded()
    }

    private companion object { val EOF = ByteArray(0) }
}

/** Result sink for an operation's output: channel 1 = stdout, 2 = stderr. */
fun interface OutputSink { fun write(channel: Int, data: ByteArray) }

object AdbOps {
    /** Runs a non-interactive `shell,v2,raw:` command; returns the remote exit code. */
    fun shell(conn: AdbConnection, commandLine: String, timeoutMs: Long, sink: OutputSink): Int {
        val deadline = System.currentTimeMillis() + timeoutMs
        conn.open("shell,v2,raw:$commandLine").use { s ->
            // Best effort: a fast command may already have sent its output, exit packet and CLSE.
            try { s.write(ShellV2.packet(ShellV2.CLOSE_STDIN)) } catch (_: IOException) {}
            val parser = ShellV2.Parser()
            while (true) {
                val left = deadline - System.currentTimeMillis()
                if (left <= 0) throw AdbTimeoutException("timeout")
                val chunk = s.read(left) ?: throw IOException(conn.failure?.let { "connection lost" } ?: "shell ended without an exit code")
                for ((id, data) in parser.feed(chunk)) when (id) {
                    ShellV2.STDOUT, ShellV2.STDERR -> sink.write(id, data)
                    ShellV2.EXIT -> return if (data.isNotEmpty()) data[0].toInt() and 0xff else 255
                }
            }
        }
    }

    /** `exec:screencap -p`: raw PNG bytes to channel 1 until the device closes the stream. */
    fun screencap(conn: AdbConnection, timeoutMs: Long, sink: OutputSink): Long {
        val deadline = System.currentTimeMillis() + timeoutMs
        var total = 0L
        conn.open("exec:screencap -p").use { s ->
            while (true) {
                val left = deadline - System.currentTimeMillis()
                if (left <= 0) throw AdbTimeoutException("timeout")
                val chunk = s.read(left) ?: break
                total += chunk.size
                sink.write(1, chunk)
            }
        }
        if (conn.closed) throw IOException("connection closed")
        return total
    }
}
