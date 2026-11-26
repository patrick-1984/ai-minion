// Spike K0 host tests: wire formats, HKDF vector, self-signed cert, and the BoringSSL SPAKE2 JNI shim.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.adbspike

import java.io.ByteArrayInputStream
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.security.KeyPairGenerator
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate
import java.security.interfaces.RSAPublicKey
import java.util.Base64
import javax.crypto.AEADBadTagException
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Test

class AdbSpikeTest {
    private fun hex(s: String) = s.chunked(2).map { it.toInt(16).toByte() }.toByteArray()

    @Test fun hkdfRfc5869Case1() {
        val okm = Hkdf.sha256(hex("0b".repeat(22)), hex("000102030405060708090a0b0c"), hex("f0f1f2f3f4f5f6f7f8f9"), 42)
        assertArrayEquals(hex("3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865"), okm)
    }

    @Test fun hkdfRfc5869Case3NoSalt() {
        val okm = Hkdf.sha256(hex("0b".repeat(22)), null, ByteArray(0), 42)
        assertArrayEquals(hex("8da4e775a563c18f715f802a063c5a31b8a11f5c5ee1879ec3454e5f3c738d2d9d201395faa4b61a96c8"), okm)
    }

    @Test fun gcmIvIsLittleEndianCounter() {
        assertArrayEquals(hex("010000000000000000000000"), PairingCipher.iv(1))
        assertArrayEquals(hex("000100000000000000000000"), PairingCipher.iv(256))
    }

    @Test fun adbPubkeyLayout() {
        val pub = KeyPairGenerator.getInstance("RSA").apply { initialize(2048) }.generateKeyPair().public as RSAPublicKey
        val enc = AdbPubkey.encode(pub)
        assertEquals(524, enc.size)
        val b = ByteBuffer.wrap(enc).order(ByteOrder.LITTLE_ENDIAN)
        assertEquals(64, b.getInt(0))
        // n0inv * n[0] == -1 mod 2^32
        assertEquals(-1, b.getInt(4) * pub.modulus.toInt())
        assertEquals(65537, b.getInt(520))
        val modulusLe = enc.copyOfRange(8, 8 + 256)
        assertEquals(pub.modulus, java.math.BigInteger(1, modulusLe.reversedArray()))
        val named = String(AdbPubkey.encodeWithName(pub, "minion@cc.nasz.minion"), Charsets.UTF_8)
        assertTrue(named.endsWith(" minion@cc.nasz.minion\u0000"))
        assertArrayEquals(enc, Base64.getDecoder().decode(named.substringBefore(' ')))
    }

    @Test fun adbMessageRoundTrip() {
        val m = AdbMessage(AdbMessage.A_OPEN, 1, 0, "shell:id -u\u0000".toByteArray())
        val bytes = m.encode()
        assertEquals("OPEN", AdbMessage.name(ByteBuffer.wrap(bytes).order(ByteOrder.LITTLE_ENDIAN).getInt(0)))
        val back = AdbMessage.read(ByteArrayInputStream(bytes))
        assertEquals(m.command, back.command); assertEquals(1, back.arg0)
        assertArrayEquals(m.data, back.data)
    }

    @Test fun pairingFrame() {
        val f = PairingPacket.frame(PairingPacket.TYPE_PEER_INFO, ByteArray(8208))
        assertArrayEquals(hex("0101") + hex("00002010"), f.copyOfRange(0, 6))
        assertEquals(8208, PairingPacket.read(ByteArrayInputStream(f), PairingPacket.TYPE_PEER_INFO).size)
        assertEquals(10, PairingPacket.EXPORTER_LABEL.toByteArray(Charsets.UTF_8).size)
    }

    @Test fun selfSignedCertParsesAndVerifies() {
        val kp = KeyPairGenerator.getInstance("RSA").apply { initialize(2048) }.generateKeyPair()
        val der = SelfSignedCert.build(kp.private, kp.public.encoded, AdbKeys.SUBJECT)
        val cert = CertificateFactory.getInstance("X.509").generateCertificate(ByteArrayInputStream(der)) as X509Certificate
        cert.verify(kp.public)
        cert.checkValidity()
        assertEquals("CN=minion@cc.nasz.minion", cert.subjectX500Principal.name)
        assertEquals(3, cert.version)
    }

    // --- BoringSSL SPAKE2 through the JNI shim (host build of libminion_spake2.so) ---------------------------

    private fun needLib() = assumeTrue(
        "set -PminionSpake2HostLib=/abs/libminion_spake2.so (host build) to run the JNI tests",
        !System.getProperty("minion.spake2.lib").isNullOrEmpty(),
    )

    private fun run(alicePw: ByteArray, bobPw: ByteArray): Pair<ByteArray, ByteArray> =
        Spake2.alice().use { a ->
            Spake2.bob().use { b ->
                val ma = a.generateMessage(alicePw)
                val mb = b.generateMessage(bobPw)
                assertEquals(32, ma.size); assertEquals(32, mb.size)
                a.processMessage(mb) to b.processMessage(ma)
            }
        }

    private fun password(code: String, exporter: ByteArray) = code.toByteArray() + exporter

    @Test fun spake2SamePasswordAgrees() {
        needLib()
        val exp = ByteArray(64) { it.toByte() }
        val (ka, kb) = run(password("123456", exp), password("123456", exp))
        assertEquals(64, ka.size)
        assertArrayEquals(ka, kb)
    }

    @Test fun spake2WrongCodeDisagreesAndPeerInfoFails() {
        needLib()
        val exp = ByteArray(64) { 7 }
        val (ka, kb) = run(password("123456", exp), password("654321", exp))
        assertFalse(ka.contentEquals(kb))
        val sealed = PairingCipher(kb).encrypt(PairingPacket.peerInfo("x".toByteArray()))
        try {
            PairingCipher(ka).decrypt(sealed); throw AssertionError("decrypt must fail with a wrong code")
        } catch (_: AEADBadTagException) {}
    }

    @Test fun spake2FullEnvelopeRoundTrip() {
        needLib()
        val exp = ByteArray(64) { (it * 3).toByte() }
        val (ka, kb) = run(password("000111", exp), password("000111", exp))
        val alice = PairingCipher(ka); val bob = PairingCipher(kb)
        val info = PairingPacket.peerInfo("key minion@cc.nasz.minion\u0000".toByteArray())
        val sealed = alice.encrypt(info)
        assertEquals(8192 + 16, sealed.size)
        assertArrayEquals(info, bob.decrypt(sealed))
        // Counters advance in step: message 1 each way.
        assertEquals("two", String(bob.decrypt(alice.encrypt("two".toByteArray()))))
        assertEquals("back", String(alice.decrypt(bob.encrypt("back".toByteArray()))))
    }

    @Test fun spake2PasswordIsWiped() {
        needLib()
        val pw = password("123456", ByteArray(64) { 1 })
        Spake2.alice().use { it.generateMessage(pw) }
        assertTrue(pw.all { it == 0.toByte() })
    }
}
