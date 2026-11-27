// Device control host tests: adb transport against a fake adbd over a socket pair, shell v2 exit codes and
// timeouts, close-all, provenance registry, endpoint choice, bridge frames, quoting and input validation.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.DataInputStream
import java.io.DataOutputStream
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.InetAddress
import java.net.ServerSocket
import java.net.Socket
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

/** A fake adbd speaking the post-TLS transport: answers OPEN per [behaviour] on its own thread. */
private class FakeAdbd(private val behaviour: (service: String, reply: (AdbMessage) -> Unit, localId: Int) -> Unit) {
    private val server = ServerSocket(0, 1, InetAddress.getLoopbackAddress())
    lateinit var peer: Socket
    val opened = java.util.concurrent.CopyOnWriteArrayList<String>()
    val closedByClient = CountDownLatch(1)
    @Volatile var stdinClosed = false

    fun connect(): AdbConnection {
        val client = Socket(server.inetAddress, server.localPort)
        peer = server.accept()
        Thread { serve(peer.getInputStream(), peer.getOutputStream()) }.apply { isDaemon = true }.start()
        return AdbConnection(client.getInputStream(), client.getOutputStream()) { client.close() }.start()
    }

    private fun serve(inp: InputStream, out: OutputStream) {
        val lock = Any()
        val reply: (AdbMessage) -> Unit = { m -> synchronized(lock) { out.write(m.encode()); out.flush() } }
        var nextRemote = 100
        try {
            while (true) {
                val m = AdbMessage.read(inp)
                when (m.command) {
                    AdbMessage.A_OPEN -> {
                        val service = String(m.data, Charsets.UTF_8).trimEnd('\u0000')
                        opened.add(service)
                        val remote = nextRemote++
                        Thread { behaviour(service, { r -> reply(AdbMessage(r.command, remote, m.arg0, r.data)) }, m.arg0) }
                            .apply { isDaemon = true }.start()
                    }
                    AdbMessage.A_WRTE -> {
                        if (ShellV2.Parser().feed(m.data).any { it.first == ShellV2.CLOSE_STDIN }) stdinClosed = true
                        reply(AdbMessage(AdbMessage.A_OKAY, m.arg1, m.arg0))
                    }
                    AdbMessage.A_CLSE -> closedByClient.countDown()
                }
            }
        } catch (_: IOException) {}
    }

    fun close() { runCatching { peer.close() }; server.close() }

    companion object {
        fun okay() = AdbMessage(AdbMessage.A_OKAY, 0, 0)
        fun wrte(data: ByteArray) = AdbMessage(AdbMessage.A_WRTE, 0, 0, data)
        fun clse() = AdbMessage(AdbMessage.A_CLSE, 0, 0)
    }
}

class DeviceControlTest {
    private var fake: FakeAdbd? = null

    @After fun tearDown() { fake?.close() }

    private fun shellAnswer(stdout: String, stderr: String, exit: Int, split: Boolean = false): (String, (AdbMessage) -> Unit, Int) -> Unit =
        { _, reply, _ ->
            reply(FakeAdbd.okay())
            val bytes = ShellV2.packet(ShellV2.STDOUT, stdout.toByteArray()) + ShellV2.packet(ShellV2.STDERR, stderr.toByteArray()) +
                ShellV2.packet(ShellV2.EXIT, byteArrayOf(exit.toByte()))
            if (split) for (b in bytes) reply(FakeAdbd.wrte(byteArrayOf(b))) else reply(FakeAdbd.wrte(bytes))
            reply(FakeAdbd.clse())
        }

    private fun collect(): Pair<OutputSink, Array<ByteArrayOutputStream>> {
        val bufs = arrayOf(ByteArrayOutputStream(), ByteArrayOutputStream(), ByteArrayOutputStream())
        return OutputSink { ch, data -> bufs[ch].write(data) } to bufs
    }

    @Test fun shellV2ReturnsRemoteExitCodeAndSeparatesStreams() {
        fake = FakeAdbd(shellAnswer("2000\n", "warn\n", 7))
        val conn = fake!!.connect()
        val (sink, out) = collect()
        assertEquals(7, AdbOps.shell(conn, "sh -c 'exit 7'", 5_000, sink))
        assertEquals("2000\n", out[1].toString())
        assertEquals("warn\n", out[2].toString())
        assertEquals("shell,v2,raw:sh -c 'exit 7'", fake!!.opened.single())
        assertTrue("stdin is closed for a non-interactive command", fake!!.stdinClosed)
        conn.close()
    }

    @Test fun shellV2PacketsSplitAcrossWritesAreReassembled() {
        fake = FakeAdbd(shellAnswer("hello", "", 0, split = true))
        val conn = fake!!.connect()
        val (sink, out) = collect()
        assertEquals(0, AdbOps.shell(conn, "echo hello", 5_000, sink))
        assertEquals("hello", out[1].toString())
    }

    @Test fun twentyFreshStreamsOnOneConnection() {
        fake = FakeAdbd(shellAnswer("2000\n", "", 0))
        val conn = fake!!.connect()
        repeat(20) { assertEquals(0, AdbOps.shell(conn, "id -u", 5_000) { _, _ -> }) }
        assertEquals(20, fake!!.opened.size)
        assertEquals(0, conn.activeStreams)
    }

    @Test fun shellTimeoutEndsTheStream() {
        fake = FakeAdbd { _, reply, _ -> reply(FakeAdbd.okay()) } // never answers
        val conn = fake!!.connect()
        val t0 = System.currentTimeMillis()
        try {
            AdbOps.shell(conn, "sleep 10", 300) { _, _ -> }
            fail("expected a timeout")
        } catch (_: AdbTimeoutException) {}
        assertTrue(System.currentTimeMillis() - t0 < 3_000)
        assertTrue("the stream was closed towards adbd", fake!!.closedByClient.await(2, TimeUnit.SECONDS))
    }

    @Test fun closeAllEndsRunningStreamsAtOnce() {
        fake = FakeAdbd { _, reply, _ -> reply(FakeAdbd.okay()) }
        val conn = fake!!.connect()
        val ended = CountDownLatch(3)
        repeat(3) {
            Thread {
                try { AdbOps.shell(conn, "sleep 600", 60_000) { _, _ -> } } catch (_: IOException) {}
                ended.countDown()
            }.start()
        }
        val deadline = System.currentTimeMillis() + 2_000
        while (conn.activeStreams < 3 && System.currentTimeMillis() < deadline) Thread.sleep(10)
        assertEquals(3, conn.activeStreams)
        conn.close()
        assertTrue("every stream ends within 2 s of close-all", ended.await(2, TimeUnit.SECONDS))
        try { conn.open("shell,v2,raw:id"); fail("closed connection must refuse new streams") } catch (_: IOException) {}
    }

    @Test fun refusedServiceIsAnError() {
        fake = FakeAdbd { _, reply, _ -> reply(FakeAdbd.clse()) }
        val conn = fake!!.connect()
        try { conn.open("exec:screencap -p"); fail("refused") } catch (_: IOException) {}
    }

    @Test fun screencapStreamsRawBytes() {
        val png = ByteArray(300_000) { (it % 251).toByte() }
        fake = FakeAdbd { _, reply, _ ->
            reply(FakeAdbd.okay())
            png.toList().chunked(65536).forEach { reply(FakeAdbd.wrte(it.toByteArray())) }
            reply(FakeAdbd.clse())
        }
        val conn = fake!!.connect()
        val (sink, out) = collect()
        assertEquals(png.size.toLong(), AdbOps.screencap(conn, 5_000, sink))
        assertArrayEquals(png, out[1].toByteArray())
        assertEquals("exec:screencap -p", fake!!.opened.single())
    }

    @Test fun lostConnectionFailsTheShell() {
        fake = FakeAdbd { _, reply, _ -> reply(FakeAdbd.okay()) }
        val conn = fake!!.connect()
        Thread { Thread.sleep(200); fake!!.peer.close() }.start()
        try { AdbOps.shell(conn, "sleep 600", 10_000) { _, _ -> }; fail("must fail") } catch (e: IOException) {
            assertFalse(e is AdbTimeoutException)
        }
    }

    // --- Provenance (design 4.4) ---------------------------------------------------------------------------------

    @Test fun provenanceAcceptsOnlyShellUidWithPendingNonce() {
        val reg = ProvenanceRegistry()
        val nonce = reg.issue()
        assertEquals(32, nonce.length)
        assertFalse("app uid refused", reg.prove(10123, nonce))
        assertFalse("root refused", reg.prove(0, nonce))
        assertFalse("wrong nonce refused", reg.prove(2000, "0".repeat(32)))
        assertFalse("missing nonce refused", reg.prove(2000, null))
        assertEquals(4, reg.denied.get())
        assertTrue(reg.prove(2000, nonce))
        assertTrue(reg.await(nonce, 100))
        assertFalse("one use only", reg.prove(2000, nonce))
        assertFalse(reg.await(nonce, 10))
    }

    @Test fun provenanceTimesOutWithoutProof() {
        val reg = ProvenanceRegistry()
        val nonce = reg.issue()
        val t0 = System.currentTimeMillis()
        assertFalse(reg.await(nonce, 200))
        assertTrue(System.currentTimeMillis() - t0 >= 150)
        assertFalse("expired nonce cannot be proven later", reg.prove(2000, nonce))
    }

    @Test fun provenanceCommandUsesTheProvider() {
        val reg = ProvenanceRegistry()
        assertEquals("content call --uri content://cc.nasz.minion.adbprobe --method prove --arg abc", reg.command("abc"))
    }

    // --- Endpoint choice (design 4.3) ----------------------------------------------------------------------------

    private val own = setOf(InetAddress.getByName("192.168.1.20"), InetAddress.getByName("127.0.0.1"))
    private fun svc(name: String, ip: String, port: Int) = AdbService(name, listOf(InetAddress.getByName(ip)), port)

    @Test fun nsdRecordsOfOtherDevicesAreIgnored() {
        val choice = EndpointChooser.choose(null, listOf(svc("tv", "192.168.1.20", 37001), svc("phone", "192.168.1.30", 41000)), own)
        assertEquals(EndpointChoice.Port(37001, "nsd"), choice)
        assertEquals(EndpointChoice.None, EndpointChooser.choose(null, listOf(svc("phone", "192.168.1.30", 41000)), own))
    }

    @Test fun severalLocalCandidatesAreAmbiguous() {
        val choice = EndpointChooser.choose(null, listOf(svc("a", "192.168.1.20", 37001), svc("b", "127.0.0.1", 37002)), own)
        assertEquals(EndpointChoice.Ambiguous(2), choice)
        // Same port twice (two interfaces) is one candidate.
        assertEquals(EndpointChoice.Port(37001, "nsd"),
            EndpointChooser.choose(null, listOf(svc("a", "192.168.1.20", 37001), svc("a2", "127.0.0.1", 37001)), own))
    }

    @Test fun manualPortWinsAndBadPortsAreIgnored() {
        assertEquals(EndpointChoice.Port(40123, "manual"), EndpointChooser.choose(40123, listOf(svc("a", "192.168.1.20", 37001)), own))
        assertEquals(EndpointChoice.Port(37001, "nsd"), EndpointChooser.choose(70000, listOf(svc("a", "192.168.1.20", 37001)), own))
        assertEquals(EndpointChoice.None, EndpointChooser.choose(null, listOf(svc("a", "192.168.1.20", 0)), own))
    }

    // --- Bridge frames, token, limits ----------------------------------------------------------------------------

    @Test fun bridgeFrameRoundTripAndLimits() {
        val buf = ByteArrayOutputStream()
        val out = DataOutputStream(buf)
        BridgeFrames.write(out, BridgeFrames.CONTROL, """{"op":"status"}""".toByteArray())
        BridgeFrames.writeData(out, BridgeFrames.STDOUT, ByteArray(BridgeFrames.MAX_PAYLOAD + 10) { 1 })
        val bytes = buf.toByteArray()
        assertArrayEquals(byteArrayOf(0, 0, 0, 16, 0), bytes.copyOfRange(0, 5))
        val inp = DataInputStream(ByteArrayInputStream(bytes))
        val (ch, payload) = BridgeFrames.read(inp)
        assertEquals(0, ch); assertEquals("""{"op":"status"}""", String(payload))
        assertEquals(BridgeFrames.MAX_PAYLOAD, BridgeFrames.read(inp).second.size)
        assertEquals(10, BridgeFrames.read(inp).second.size)
        val bad = DataInputStream(ByteArrayInputStream(byteArrayOf(0x7f, 0, 0, 0, 0)))
        try { BridgeFrames.read(bad); fail("oversized frame") } catch (_: IOException) {}
        val badChannel = DataInputStream(ByteArrayInputStream(byteArrayOf(0, 0, 0, 1, 9)))
        try { BridgeFrames.read(badChannel); fail("bad channel") } catch (_: IOException) {}
    }

    @Test fun tokenIsRandomAndComparedExactly() {
        val a = BridgeToken.generate(); val b = BridgeToken.generate()
        assertEquals(64, a.length)
        assertFalse(a == b)
        assertTrue(BridgeToken.matches(a, a))
        assertFalse(BridgeToken.matches(a, b))
        assertFalse(BridgeToken.matches(a, null))
        assertFalse(BridgeToken.matches(a, ""))
        assertEquals("MINION_ADB_BRIDGE_TOKEN=$a\n", BridgeToken.stdinLine(a))
    }

    @Test fun timeoutsAreBounded() {
        assertEquals(120_000L, OpLimits.timeout(null))
        assertEquals(120_000L, OpLimits.timeout(0))
        assertEquals(3_000L, OpLimits.timeout(3_000))
        assertEquals(3_600_000L, OpLimits.timeout(99_999_999))
    }

    @Test fun commandLineQuoting() {
        assertEquals("ls | head", ShellQuote.commandLine(listOf("ls | head")))
        assertEquals("'sh' '-c' 'exit 7'", ShellQuote.commandLine(listOf("sh", "-c", "exit 7")))
        assertEquals("'echo' 'it'\\''s'", ShellQuote.commandLine(listOf("echo", "it's")))
        try { ShellQuote.commandLine(emptyList()); fail("empty") } catch (_: IllegalArgumentException) {}
        try { ShellQuote.commandLine(listOf("a\u0000b", "c")); fail("NUL") } catch (_: IllegalArgumentException) {}
    }

    @Test fun inputArgumentsAreValidated() {
        assertEquals("'input' 'keyevent' 'KEYCODE_HOME'", InputArgs.commandLine(listOf("keyevent", "KEYCODE_HOME")))
        assertEquals("'input' 'tap' '100' '200'", InputArgs.commandLine(listOf("tap", "100", "200")))
        assertEquals("'input' 'swipe' '1' '2' '3' '4' '300'", InputArgs.commandLine(listOf("swipe", "1", "2", "3", "4", "300")))
        assertEquals("'input' 'text' 'a'\\''b; rm'", InputArgs.commandLine(listOf("text", "a'b; rm")))
        assertNull(InputArgs.commandLine(listOf("keyevent", "HOME; reboot")))
        assertNull(InputArgs.commandLine(listOf("tap", "1")))
        assertNull(InputArgs.commandLine(listOf("tap", "-1", "2")))
        assertNull(InputArgs.commandLine(listOf("roll", "1", "2")))
        assertNull(InputArgs.commandLine(listOf("text", "line\nbreak")))
        assertNull(InputArgs.commandLine(emptyList()))
    }

    @Test fun shellV2ParserRejectsBadLength() {
        val p = ShellV2.Parser()
        try { p.feed(byteArrayOf(1, 0xff.toByte(), 0xff.toByte(), 0xff.toByte(), 0x7f)); fail("bad length") } catch (_: IOException) {}
        val q = ShellV2.Parser()
        assertTrue(q.feed(byteArrayOf(1, 3, 0)).isEmpty())
        assertEquals(3, q.pending)
    }

    @Test fun workerBuiltInputCommandsParseBackAndAnythingElseIsRefused() {
        // The Go Worker's InputCommand output (bare safe words, '...' quoting).
        assertEquals(listOf("input", "keyevent", "KEYCODE_HOME"), ShellWords.split("input keyevent KEYCODE_HOME"))
        assertEquals(listOf("input", "text", "it's%sok"), ShellWords.split("input text 'it'\\''s%sok'"))
        assertEquals(listOf("input", "swipe", "1", "2", "3", "4", "300"), ShellWords.split("input swipe 1 2 3 4 300"))
        assertNull(ShellWords.split("input keyevent 3; reboot"))
        assertNull(ShellWords.split("input text \"\$(id)\""))
        assertNull(ShellWords.split("input text 'unbalanced"))
        assertNull(ShellWords.split("input tap 1 2 && rm -rf /"))
        val args = ShellWords.split("input text 'a b'")!!.drop(1)
        assertEquals("'input' 'text' 'a b'", InputArgs.commandLine(args))
    }
}
