// Spike K0: adb wire formats (pure JVM, unit-tested on the host).
// SPDX-License-Identifier: Apache-2.0
//
// Written for Minion from the AOSP adb sources (packages/modules/adb: protocol.txt, pairing_connection.cpp,
// pairing_auth.cpp, aes_128_gcm.cpp, libcrypto_utils/android_pubkey.cpp; Apache-2.0), cross-read with
// libadb-android 3.1.1 (used under its Apache-2.0 option). No code of spake2-java is used.
package cc.nasz.minion.adbspike

import java.io.DataInputStream
import java.io.EOFException
import java.io.IOException
import java.io.InputStream
import java.math.BigInteger
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.security.interfaces.RSAPublicKey
import java.util.Base64
import javax.crypto.Cipher
import javax.crypto.Mac
import javax.crypto.spec.GCMParameterSpec
import javax.crypto.spec.SecretKeySpec

/** Android adb RSA public key format (android_pubkey.cpp) and the "<base64> <name>\0" PeerInfo payload. */
object AdbPubkey {
    const val MODULUS_BYTES = 256
    const val ENCODED_SIZE = 3 * 4 + 2 * MODULUS_BYTES // 524

    fun encode(key: RSAPublicKey): ByteArray {
        val n = key.modulus
        require(n.bitLength() == MODULUS_BYTES * 8) { "adb needs an RSA-2048 key" }
        val r32 = BigInteger.ONE.shiftLeft(32)
        val n0inv = r32.subtract(n.mod(r32).modInverse(r32))
        val rr = BigInteger.ONE.shiftLeft(MODULUS_BYTES * 8).modPow(BigInteger.valueOf(2), n)
        return ByteBuffer.allocate(ENCODED_SIZE).order(ByteOrder.LITTLE_ENDIAN)
            .putInt(MODULUS_BYTES / 4)
            .putInt(n0inv.toInt())
            .put(littleEndian(n))
            .put(littleEndian(rr))
            .putInt(key.publicExponent.toInt())
            .array()
    }

    fun encodeWithName(key: RSAPublicKey, name: String): ByteArray =
        Base64.getEncoder().encode(encode(key)) + " $name\u0000".toByteArray(Charsets.UTF_8)

    private fun littleEndian(v: BigInteger): ByteArray {
        val be = v.toByteArray().let { if (it.size > MODULUS_BYTES) it.copyOfRange(it.size - MODULUS_BYTES, it.size) else it }
        val out = ByteArray(MODULUS_BYTES)
        for (i in be.indices) out[i] = be[be.size - 1 - i]
        return out
    }
}

/** HKDF-SHA256 (RFC 5869) with the platform HmacSHA256. */
object Hkdf {
    fun sha256(ikm: ByteArray, salt: ByteArray?, info: ByteArray, length: Int): ByteArray {
        val mac = Mac.getInstance("HmacSHA256")
        mac.init(SecretKeySpec(if (salt == null || salt.isEmpty()) ByteArray(32) else salt, "HmacSHA256"))
        val prk = mac.doFinal(ikm)
        mac.init(SecretKeySpec(prk, "HmacSHA256"))
        val out = ByteArray(length)
        var t = ByteArray(0)
        var pos = 0
        var counter = 1
        while (pos < length) {
            mac.update(t); mac.update(info); mac.update(counter.toByte())
            t = mac.doFinal()
            val n = minOf(t.size, length - pos)
            System.arraycopy(t, 0, out, pos, n)
            pos += n; counter++
        }
        prk.fill(0)
        return out
    }
}

/** AES-128-GCM envelope of adb pairing (aes_128_gcm.cpp): key = HKDF(spake key), IV = LE 64-bit counter + 4 zero bytes. */
class PairingCipher(spakeKey: ByteArray) {
    private val key = Hkdf.sha256(spakeKey, null, INFO, 16)
    private var encCounter = 0L
    private var decCounter = 0L

    fun encrypt(plain: ByteArray): ByteArray = run(Cipher.ENCRYPT_MODE, plain, encCounter++)
    fun decrypt(sealed: ByteArray): ByteArray = run(Cipher.DECRYPT_MODE, sealed, decCounter++)

    private fun run(mode: Int, input: ByteArray, counter: Long): ByteArray {
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(mode, SecretKeySpec(key, "AES"), GCMParameterSpec(128, iv(counter)))
        return c.doFinal(input)
    }

    fun wipe() = key.fill(0)

    companion object {
        val INFO = "adb pairing_auth aes-128-gcm key".toByteArray(Charsets.UTF_8)
        fun iv(counter: Long): ByteArray = ByteBuffer.allocate(12).order(ByteOrder.LITTLE_ENDIAN).putLong(counter).array()
    }
}

/** Pairing packet framing: version(1) type(1) payload size (4, big-endian). */
object PairingPacket {
    const val VERSION: Byte = 1
    const val TYPE_SPAKE2_MSG: Byte = 0
    const val TYPE_PEER_INFO: Byte = 1
    const val PEER_INFO_SIZE = 1 shl 13
    const val PEER_RSA_PUB_KEY: Byte = 0
    const val MAX_PAYLOAD = 2 * PEER_INFO_SIZE
    const val EXPORTER_LABEL = "adb-label\u0000" // 10 bytes incl. NUL, as AOSP sizeof("adb-label")
    const val EXPORTER_SIZE = 64

    fun frame(type: Byte, payload: ByteArray): ByteArray =
        ByteBuffer.allocate(6 + payload.size).order(ByteOrder.BIG_ENDIAN)
            .put(VERSION).put(type).putInt(payload.size).put(payload).array()

    fun read(input: InputStream, expectedType: Byte): ByteArray {
        val d = DataInputStream(input)
        val version = d.readByte()
        val type = d.readByte()
        val size = d.readInt()
        if (version != VERSION) throw IOException("pairing: unsupported header version $version")
        if (type != expectedType) throw IOException("pairing: expected packet type $expectedType, got $type")
        if (size <= 0 || size > MAX_PAYLOAD) throw IOException("pairing: bad payload size $size")
        return ByteArray(size).also { d.readFully(it) }
    }

    fun peerInfo(adbPublicKeyWithName: ByteArray): ByteArray {
        require(adbPublicKeyWithName.size <= PEER_INFO_SIZE - 1)
        return ByteArray(PEER_INFO_SIZE).also {
            it[0] = PEER_RSA_PUB_KEY
            System.arraycopy(adbPublicKeyWithName, 0, it, 1, adbPublicKeyWithName.size)
        }
    }
}

/** adb transport message (protocol.txt): 6 little-endian uint32 + payload. */
class AdbMessage(val command: Int, val arg0: Int, val arg1: Int, val data: ByteArray = ByteArray(0)) {
    fun encode(): ByteArray =
        ByteBuffer.allocate(HEADER + data.size).order(ByteOrder.LITTLE_ENDIAN)
            .putInt(command).putInt(arg0).putInt(arg1).putInt(data.size)
            .putInt(checksum(data)).putInt(command xor -1).put(data).array()

    override fun toString() = "AdbMessage(${name(command)}, $arg0, $arg1, ${data.size} bytes)"

    companion object {
        const val HEADER = 24
        const val A_CNXN = 0x4e584e43
        const val A_STLS = 0x534c5453
        const val A_AUTH = 0x48545541
        const val A_OPEN = 0x4e45504f
        const val A_OKAY = 0x59414b4f
        const val A_WRTE = 0x45545257
        const val A_CLSE = 0x45534c43
        const val VERSION = 0x01000001 // skip-checksum version
        const val STLS_VERSION = 0x01000000
        const val MAX_DATA = 1024 * 1024

        fun checksum(data: ByteArray): Int = data.fold(0) { acc, b -> acc + (b.toInt() and 0xff) }

        fun read(input: InputStream): AdbMessage {
            val h = ByteArray(HEADER)
            DataInputStream(input).readFully(h)
            val b = ByteBuffer.wrap(h).order(ByteOrder.LITTLE_ENDIAN)
            val cmd = b.int; val a0 = b.int; val a1 = b.int; val len = b.int; b.int; val magic = b.int
            if (magic != cmd xor -1) throw IOException("adb: bad magic for ${name(cmd)}")
            if (len < 0 || len > MAX_DATA) throw IOException("adb: bad payload length $len")
            val data = ByteArray(len)
            try { DataInputStream(input).readFully(data) } catch (e: EOFException) { throw IOException("adb: short payload", e) }
            return AdbMessage(cmd, a0, a1, data)
        }

        fun name(cmd: Int): String =
            String(ByteBuffer.allocate(4).order(ByteOrder.LITTLE_ENDIAN).putInt(cmd).array(), Charsets.US_ASCII)
    }
}
