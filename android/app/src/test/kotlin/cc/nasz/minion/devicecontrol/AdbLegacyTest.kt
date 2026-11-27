// Device control host tests: the legacy adb transport against a fake legacy adbd over a socket pair (CNXN, AUTH
// token/signature/public key, the TV dialog, OPEN/OKAY/WRTE/CLSE, shell v2 exit codes, provenance on this transport).
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.InetAddress
import java.net.ServerSocket
import java.net.Socket
import java.security.KeyPairGenerator
import java.security.SecureRandom
import java.security.interfaces.RSAPublicKey
import java.security.spec.RSAKeyGenParameterSpec
import java.util.Base64
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import javax.crypto.Cipher
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

/**
 * A fake legacy adbd (ro.adb.secure=1): AUTH TOKEN after CNXN, checks SIGNATURE against its adb_keys with plain
 * RSA (independent of the client code), shows "the dialog" on RSAPUBLICKEY and waits for [allow]/[deny].
 */
private class FakeLegacyAdbd(
    private val registry: ProvenanceRegistry,
    private val proveAs: Int = ProvenanceRegistry.SHELL_UID,
    private val answerStls: Boolean = false,
    private val tokenSize: Int = AdbAuth.TOKEN_SIZE,
) {
    val server = ServerSocket(0, 4, InetAddress.getLoopbackAddress())
    val authorized = CopyOnWriteArrayList<RSAPublicKey>()
    val events = CopyOnWriteArrayList<String>()
    val offeredKeys = CopyOnWriteArrayList<ByteArray>()
    val dialogShown = CountDownLatch(1)
    private val decision = CountDownLatch(1)
    @Volatile private var allowed = false
    private val peers = CopyOnWriteArrayList<Socket>()

    fun allow() { allowed = true; decision.countDown() }
    fun deny() { allowed = false; decision.countDown() }

    fun dial(): Socket {
        val client = Socket(server.inetAddress, server.localPort)
        val peer = server.accept()
        peers.add(peer)
        Thread { runCatching { serve(peer.getInputStream(), peer.getOutputStream(), peer) } }.apply { isDaemon = true }.start()
        return client
    }

    private fun send(out: OutputStream, m: AdbMessage) = synchronized(out) { out.write(m.encode()); out.flush() }

    private fun token() = ByteArray(tokenSize).also(SecureRandom()::nextBytes)

    /** adbd's RSA_verify(NID_sha1, token, 20, sig): s^e mod n, then the PKCS#1 v1.5 block must hold DigestInfo(token). */
    private fun signatureOk(key: RSAPublicKey, token: ByteArray, sig: ByteArray): Boolean = runCatching {
        val c = Cipher.getInstance("RSA/ECB/PKCS1Padding")
        c.init(Cipher.DECRYPT_MODE, key)
        val di = c.doFinal(sig)
        di.size == 35 && di.copyOfRange(15, 35).contentEquals(token) &&
            di.copyOfRange(0, 15).contentEquals(byteArrayOf(0x30, 0x21, 0x30, 0x09, 0x06, 0x05, 0x2b, 0x0e, 0x03, 0x02, 0x1a, 0x05, 0x00, 0x04, 0x14))
    }.getOrDefault(false)

    private fun serve(inp: InputStream, out: OutputStream, peer: Socket) {
        val hello = AdbMessage.read(inp)
        check(hello.command == AdbMessage.A_CNXN && String(hello.data).contains("shell_v2"))
        events.add("CNXN")
        if (answerStls) { send(out, AdbMessage(AdbMessage.A_STLS, AdbMessage.STLS_VERSION, 0)); return }
        var token = token()
        send(out, AdbMessage(AdbMessage.A_AUTH, AdbAuth.TOKEN, 0, token))
        handshake@ while (true) {
            val m = AdbMessage.read(inp)
            check(m.command == AdbMessage.A_AUTH)
            when (m.arg0) {
                AdbAuth.SIGNATURE -> {
                    events.add("SIGNATURE")
                    if (authorized.any { signatureOk(it, token, m.data) }) break@handshake
                    token = token()
                    send(out, AdbMessage(AdbMessage.A_AUTH, AdbAuth.TOKEN, 0, token))
                }
                AdbAuth.RSAPUBLICKEY -> {
                    events.add("RSAPUBLICKEY")
                    offeredKeys.add(m.data)
                    dialogShown.countDown()
                    decision.await(10, TimeUnit.SECONDS)
                    if (!allowed) { peer.close(); return }
                    break@handshake
                }
            }
        }
        send(out, AdbMessage(AdbMessage.A_CNXN, AdbMessage.VERSION, AdbMessage.MAX_DATA, "device::features=shell_v2,cmd\u0000".toByteArray()))
        var nextRemote = 500
        while (true) {
            val m = AdbMessage.read(inp)
            when (m.command) {
                AdbMessage.A_OPEN -> {
                    val service = String(m.data, Charsets.UTF_8).trimEnd('\u0000')
                    events.add("OPEN $service")
                    val remote = nextRemote++
                    val local = m.arg0
                    send(out, AdbMessage(AdbMessage.A_OKAY, remote, local))
                    Thread { runCatching { shell(service, remote, local, out) } }.apply { isDaemon = true }.start()
                }
                AdbMessage.A_WRTE -> send(out, AdbMessage(AdbMessage.A_OKAY, m.arg1, m.arg0))
                AdbMessage.A_CLSE -> events.add("CLSE")
                AdbMessage.A_OKAY -> {}
            }
        }
    }

    private fun shell(service: String, remote: Int, local: Int, out: OutputStream) {
        val cmd = service.removePrefix("shell,v2,raw:")
        val exit = when {
            cmd.startsWith("content call --uri content://${ProvenanceRegistry.AUTHORITY}") -> {
                // The provider call made by the device's shell user (uid 2000 on a real adbd).
                if (registry.prove(proveAs, cmd.substringAfter("--arg ").trim())) 0 else 1
            }
            cmd == "id -u" -> { wrte(out, remote, local, ShellV2.packet(ShellV2.STDOUT, "2000\n".toByteArray())); 0 }
            cmd == "sh -c 'exit 7'" -> { wrte(out, remote, local, ShellV2.packet(ShellV2.STDERR, "seven\n".toByteArray())); 7 }
            else -> 127
        }
        wrte(out, remote, local, ShellV2.packet(ShellV2.EXIT, byteArrayOf(exit.toByte())))
        send(out, AdbMessage(AdbMessage.A_CLSE, remote, local))
    }

    private fun wrte(out: OutputStream, remote: Int, local: Int, data: ByteArray) =
        send(out, AdbMessage(AdbMessage.A_WRTE, remote, local, data))

    fun close() { for (p in peers) runCatching { p.close() }; server.close() }
}

class AdbLegacyTest {
    private val keys = KeyPairGenerator.getInstance("RSA").apply { initialize(RSAKeyGenParameterSpec(2048, RSAKeyGenParameterSpec.F4)) }
        .generateKeyPair()
    private val pub = keys.public as RSAPublicKey
    private val registry = ProvenanceRegistry()
    private var fake: FakeLegacyAdbd? = null

    @After fun tearDown() { fake?.close() }

    /** The bridge's provenance check (AdbBridge.verify) on a JVM: the nonce must be proven by uid 2000. */
    private fun verify(conn: AdbConnection) {
        val nonce = registry.issue()
        val exit = try { AdbOps.shell(conn, registry.command(nonce), 5_000) { _, _ -> } } catch (_: IOException) { -1 }
        if (exit != 0 || !registry.await(nonce, 1_000)) throw BridgeException("endpoint_unverified", "provenance failed")
    }

    private fun connector(f: FakeLegacyAdbd, dial: () -> Socket = { f.dial() }) =
        LegacyConnector(dial, keys.private, pub, "minion@test", ::verify, 5_000)

    private fun shell(conn: AdbConnection, cmd: String): Triple<Int, String, String> {
        val out = StringBuilder(); val err = StringBuilder()
        val code = AdbOps.shell(conn, cmd, 5_000) { ch, d -> (if (ch == 1) out else err).append(String(d)) }
        return Triple(code, out.toString(), err.toString())
    }

    @Test fun tokenSignatureIsPkcs1Sha1DigestInfoAndVerifiesWithThePublicKey() {
        val token = ByteArray(20) { it.toByte() }
        val sig = AdbAuth.sign(keys.private, token)
        assertEquals(256, sig.size)
        assertTrue(AdbAuth.verify(pub, token, sig))
        assertFalse(AdbAuth.verify(pub, ByteArray(20), sig))
        // Independent check: the PKCS#1 v1.5 block under the signature is the SHA-1 DigestInfo of the raw token.
        val di = Cipher.getInstance("RSA/ECB/PKCS1Padding").apply { init(Cipher.DECRYPT_MODE, pub) }.doFinal(sig)
        assertArrayEquals(AdbAuth.digestInfo(token), di)
        // The raw fallback (EMSA block + NoPadding) gives the same deterministic signature.
        val raw = Cipher.getInstance("RSA/ECB/NoPadding").apply { init(Cipher.ENCRYPT_MODE, keys.private) }
            .doFinal(AdbAuth.emsaPkcs1(AdbAuth.digestInfo(token), 256))
        assertArrayEquals(sig, raw)
    }

    @Test fun knownKeyAuthenticatesSilentlyAndRunsShellWithExitCodes() {
        val f = FakeLegacyAdbd(registry).also { fake = it }
        f.authorized.add(pub)
        val a = connector(f).attempt()
        assertTrue(a is LegacyAttempt.Ready)
        val conn = (a as LegacyAttempt.Ready).conn
        assertEquals(listOf("CNXN", "SIGNATURE"), f.events.take(2))
        assertFalse(f.events.contains("RSAPUBLICKEY"))
        assertTrue(f.events.any { it.startsWith("OPEN shell,v2,raw:content call") })
        assertEquals(Triple(0, "2000\n", ""), shell(conn, "id -u"))
        assertEquals(Triple(7, "", "seven\n"), shell(conn, "sh -c 'exit 7'"))
        assertEquals(127, shell(conn, "nope").first)
        conn.close()
    }

    @Test fun unknownKeyOffersThePublicKeyAndConnectsAfterTheOwnerAllows() {
        val f = FakeLegacyAdbd(registry).also { fake = it }
        val a = connector(f).attempt()
        assertTrue(a is LegacyAttempt.AwaitingUser)
        // The client returns once it has sent the key; the fake records it on its own thread.
        assertTrue(f.dialogShown.await(5, TimeUnit.SECONDS))
        assertEquals(listOf("CNXN", "SIGNATURE", "RSAPUBLICKEY"), f.events.toList())
        // AUTH_RSAPUBLICKEY = base64(android RSA pubkey struct) + " name" + NUL (client/auth.cpp).
        val offered = String(f.offeredKeys.single(), Charsets.UTF_8)
        assertTrue(offered.endsWith(" minion@test\u0000"))
        assertArrayEquals(AdbPubkey.encode(pub), Base64.getDecoder().decode(offered.substringBefore(' ')))
        f.allow()
        val conn = (a as LegacyAttempt.AwaitingUser).finish(5_000)
        assertEquals(0, shell(conn, "id -u").first)
        conn.close()
        // "Always allow": the next connection is silent.
        f.authorized.add(pub)
        f.events.clear()
        val again = connector(f).attempt()
        assertTrue(again is LegacyAttempt.Ready)
        assertFalse(f.events.contains("RSAPUBLICKEY"))
        (again as LegacyAttempt.Ready).conn.close()
    }

    @Test fun ownerDenyingOrNotAnsweringIsNeedsAuthorization() {
        val f = FakeLegacyAdbd(registry).also { fake = it }
        val waiting = connector(f).attempt() as LegacyAttempt.AwaitingUser
        try { waiting.finish(200); fail("no answer must time out") } catch (e: BridgeException) {
            assertEquals(LegacyConnector.NEEDS_AUTH, e.code)
        }
        val denied = connector(f).attempt() as LegacyAttempt.AwaitingUser
        f.deny()
        try { denied.finish(5_000); fail("deny must fail") } catch (e: BridgeException) {
            assertEquals(LegacyConnector.NEEDS_AUTH, e.code)
        }
    }

    @Test fun failedProvenanceRefusesTheLegacyEndpoint() {
        val f = FakeLegacyAdbd(registry, proveAs = 10_123).also { fake = it } // an app uid, not shell
        f.authorized.add(pub)
        try { connector(f).attempt(); fail("must not connect") } catch (e: BridgeException) {
            assertEquals("endpoint_unverified", e.code)
        }
        assertTrue(registry.denied.get() >= 1)
        assertFalse(f.events.any { it.contains("id -u") })
    }

    @Test fun nothingListeningOrATlsPortIsAbsent() {
        val f = FakeLegacyAdbd(registry).also { fake = it }
        assertTrue(connector(f) { throw java.net.ConnectException("refused") }.attempt() === LegacyAttempt.Absent)
        val tls = FakeLegacyAdbd(registry, answerStls = true)
        try {
            assertTrue(LegacyConnector({ tls.dial() }, keys.private, pub, "minion@test", ::verify).attempt() === LegacyAttempt.Absent)
        } finally { tls.close() }
    }

    @Test fun badTokenSizeIsNotAnAdbd() {
        val f = FakeLegacyAdbd(registry, tokenSize = 16).also { fake = it }
        try { connector(f).attempt(); fail("must refuse") } catch (e: BridgeException) {
            assertEquals("endpoint_unverified", e.code)
        }
    }
}
