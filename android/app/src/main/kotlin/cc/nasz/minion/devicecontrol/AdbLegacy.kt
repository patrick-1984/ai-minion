// Device control: the legacy (non-TLS) adb transport on 127.0.0.1:5555. Pure JVM; unit-tested against a fake
// legacy adbd over a socket pair.
// SPDX-License-Identifier: Apache-2.0
//
// Written for Minion from AOSP packages/modules/adb (Apache-2.0): protocol.txt (CNXN, AUTH), client/auth.cpp
// (adb_auth_sign: RSA_sign(NID_sha1, token, 20); send_auth_publickey: "<base64 android pubkey> <name>" + NUL) and
// daemon/auth.cpp (adbd_auth_verify: RSA_verify(NID_sha1, token, 20, sig) against every key in adb_keys; an unknown
// key gets a new AUTH TOKEN, the public key then opens "Allow debugging?" on the device). No other adb code is used.
package cc.nasz.minion.devicecontrol

import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.math.BigInteger
import java.net.Socket
import java.net.SocketTimeoutException
import java.security.PrivateKey
import java.security.Signature
import java.security.interfaces.RSAPublicKey
import javax.crypto.Cipher

/** AUTH of the legacy adb handshake: the token signature adbd checks (RSA PKCS#1 v1.5, SHA-1 DigestInfo). */
object AdbAuth {
    const val TOKEN = 1
    const val SIGNATURE = 2
    const val RSAPUBLICKEY = 3
    const val TOKEN_SIZE = 20

    /** DER DigestInfo prefix of SHA-1 (RFC 8017 9.2, note 1): what BoringSSL RSA_sign(NID_sha1, ...) prepends. */
    private val SHA1_DIGEST_INFO = byteArrayOf(
        0x30, 0x21, 0x30, 0x09, 0x06, 0x05, 0x2b, 0x0e, 0x03, 0x02, 0x1a, 0x05, 0x00, 0x04, 0x14,
    )

    /** The 35 bytes that are PKCS#1 v1.5 padded and signed: the token is used as the SHA-1 digest, not hashed. */
    fun digestInfo(token: ByteArray): ByteArray {
        if (token.size != TOKEN_SIZE) throw IOException("adb: AUTH token of ${token.size} bytes, expected $TOKEN_SIZE")
        return SHA1_DIGEST_INFO + token
    }

    /**
     * Signs [token] as adbd expects. "NONEwithRSA" pads the DigestInfo itself (AndroidKeyStore: KM_DIGEST_NONE +
     * KM_PAD_RSA_PKCS1_1_5_SIGN, both in [AdbKeys.keySpec]); if a provider refuses that, the EMSA-PKCS1-v1_5 block
     * is built here and signed raw with "RSA/ECB/NoPadding" (the operation the TLS path already uses).
     */
    fun sign(key: PrivateKey, token: ByteArray): ByteArray {
        val di = digestInfo(token)
        return try {
            Signature.getInstance("NONEwithRSA").run { initSign(key); update(di); sign() }
        } catch (_: Exception) {
            val c = Cipher.getInstance("RSA/ECB/NoPadding")
            c.init(Cipher.ENCRYPT_MODE, key)
            c.doFinal(emsaPkcs1(di, AdbPubkey.MODULUS_BYTES))
        }
    }

    /** EMSA-PKCS1-v1_5 (RFC 8017 9.2): 00 01 FF.. 00 || DigestInfo, [k] bytes. */
    fun emsaPkcs1(digestInfo: ByteArray, k: Int): ByteArray {
        require(digestInfo.size + 11 <= k)
        val em = ByteArray(k) { 0xff.toByte() }
        em[0] = 0; em[1] = 1
        em[k - digestInfo.size - 1] = 0
        System.arraycopy(digestInfo, 0, em, k - digestInfo.size, digestInfo.size)
        return em
    }

    /** adbd's check, from first principles: sig^e mod n == EMSA-PKCS1-v1_5(DigestInfo(token)). */
    fun verify(key: RSAPublicKey, token: ByteArray, signature: ByteArray): Boolean {
        val k = (key.modulus.bitLength() + 7) / 8
        if (signature.size != k || token.size != TOKEN_SIZE) return false
        val s = BigInteger(1, signature)
        if (s >= key.modulus) return false
        val em = s.modPow(key.publicExponent, key.modulus)
        return em == BigInteger(1, emsaPkcs1(digestInfo(token), k))
    }
}

/** One legacy handshake on a raw socket (protocol.txt): CNXN, then AUTH TOKEN/SIGNATURE/RSAPUBLICKEY, then CNXN. */
class LegacyHandshake(
    private val input: InputStream,
    private val output: OutputStream,
    private val signToken: (ByteArray) -> ByteArray,
    private val publicKeyWithName: ByteArray,
) {
    sealed class Step {
        /** adbd accepted the key (or needs none): its CNXN. */
        class Connected(val cnxn: AdbMessage) : Step()
        /** adbd does not know the key: the public key was sent and "Allow debugging?" is on the device's screen. */
        object NeedsUser : Step()
        /** The port speaks Wireless debugging (STLS), not the legacy protocol. */
        object Tls : Step()
    }

    private var signed = false

    fun start(): Step {
        send(AdbMessage(AdbMessage.A_CNXN, AdbMessage.VERSION, AdbMessage.MAX_DATA, BANNER))
        while (true) {
            val m = AdbMessage.read(input)
            when (m.command) {
                AdbMessage.A_CNXN -> return Step.Connected(m)
                AdbMessage.A_STLS -> return Step.Tls
                AdbMessage.A_AUTH -> {
                    if (m.arg0 != AdbAuth.TOKEN) throw IOException("adb: unexpected AUTH type ${m.arg0}")
                    if (!signed) {
                        signed = true
                        send(AdbMessage(AdbMessage.A_AUTH, AdbAuth.SIGNATURE, 0, signToken(m.data)))
                    } else {
                        // Our one key was refused: offer it (client/auth.cpp send_auth_publickey).
                        send(AdbMessage(AdbMessage.A_AUTH, AdbAuth.RSAPUBLICKEY, 0, publicKeyWithName))
                        return Step.NeedsUser
                    }
                }
                else -> throw IOException("adb: unexpected ${AdbMessage.name(m.command)} during the handshake")
            }
        }
    }

    /** After [Step.NeedsUser]: the device's CNXN once the owner allowed it. Read timeout and EOF are the caller's. */
    fun awaitUser(): AdbMessage {
        while (true) {
            val m = AdbMessage.read(input)
            when (m.command) {
                AdbMessage.A_CNXN -> return m
                // Some adbd versions ask again once the key is known: the key is ours, so sign.
                AdbMessage.A_AUTH -> if (m.arg0 == AdbAuth.TOKEN) {
                    send(AdbMessage(AdbMessage.A_AUTH, AdbAuth.SIGNATURE, 0, signToken(m.data)))
                } else throw IOException("adb: unexpected AUTH type ${m.arg0}")
                else -> throw IOException("adb: unexpected ${AdbMessage.name(m.command)} while waiting for authorization")
            }
        }
    }

    private fun send(m: AdbMessage) { output.write(m.encode()); output.flush() }

    companion object {
        /** Same banner as the TLS path (AdbBridge.BANNER): shell v2 is needed for exit codes. */
        val BANNER = "host::features=shell_v2,cmd\u0000".toByteArray(Charsets.UTF_8)
    }
}

/** Outcome of one attempt on the legacy port. */
sealed class LegacyAttempt {
    /** Connected, authenticated silently and provenance-checked. */
    class Ready(val conn: AdbConnection) : LegacyAttempt()
    /** Nothing listens there, or it is a TLS (Wireless debugging) port: try the TLS path. */
    object Absent : LegacyAttempt()
    /** The device shows "Allow debugging?". [finish] waits for the owner, then runs the provenance check. */
    class AwaitingUser internal constructor(
        private val wait: (Long) -> AdbConnection,
        private val abort: () -> Unit,
    ) : LegacyAttempt() {
        fun finish(timeoutMs: Long): AdbConnection = wait(timeoutMs)
        fun cancel() = abort()
    }
}

/**
 * Legacy adb client: [dial] opens the loopback socket (AdbBridge: 127.0.0.1:5555 only, never another address),
 * [verify] is the provenance check (design 4.4) every new connection must pass before anything else is sent.
 */
class LegacyConnector(
    private val dial: () -> Socket,
    private val key: PrivateKey,
    publicKey: RSAPublicKey,
    keyName: String,
    private val verify: (AdbConnection) -> Unit,
    private val handshakeTimeoutMs: Int = 5_000,
) {
    private val pubkey = AdbPubkey.encodeWithName(publicKey, keyName)

    fun attempt(): LegacyAttempt {
        val s = try { dial() } catch (_: IOException) { return LegacyAttempt.Absent }
        try {
            s.soTimeout = handshakeTimeoutMs
            val hs = LegacyHandshake(s.getInputStream(), s.getOutputStream(), { AdbAuth.sign(key, it) }, pubkey)
            return when (val step = hs.start()) {
                is LegacyHandshake.Step.Connected -> LegacyAttempt.Ready(adopt(s))
                LegacyHandshake.Step.Tls -> { runCatching { s.close() }; LegacyAttempt.Absent }
                LegacyHandshake.Step.NeedsUser -> LegacyAttempt.AwaitingUser({ timeoutMs ->
                    try {
                        s.soTimeout = timeoutMs.coerceIn(1, Int.MAX_VALUE.toLong()).toInt()
                        hs.awaitUser()
                    } catch (_: SocketTimeoutException) {
                        runCatching { s.close() }
                        throw BridgeException(NEEDS_AUTH, NOT_CONFIRMED)
                    } catch (_: IOException) {
                        runCatching { s.close() }
                        throw BridgeException(NEEDS_AUTH, NOT_ALLOWED)
                    }
                    adopt(s)
                }, { runCatching { s.close() } })
            }
        } catch (e: BridgeException) {
            runCatching { s.close() }
            throw e
        } catch (_: Exception) {
            runCatching { s.close() }
            throw BridgeException("endpoint_unverified", "The endpoint on 127.0.0.1:$PORT does not finish the adb handshake.")
        }
    }

    private fun adopt(s: Socket): AdbConnection {
        s.soTimeout = 0
        val c = AdbConnection(s.getInputStream(), s.getOutputStream()) { runCatching { s.close() } }.start()
        try { verify(c) } catch (e: Exception) { c.close(); throw e }
        return c
    }

    companion object {
        const val PORT = 5555
        const val NEEDS_AUTH = "needs_authorization"
        const val CONFIRM = "Confirm \"Allow debugging?\" on the TV and tick \"Always allow from this computer\"."
        const val NOT_CONFIRMED = "Not confirmed on the TV in time. Use device control again and confirm \"Allow debugging?\"."
        const val NOT_ALLOWED = "Debugging was not allowed on the TV. Use device control again and choose Allow."
    }
}
