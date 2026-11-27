// Device control: the adb client (pairing and TLS connect), loopback only.
// SPDX-License-Identifier: Apache-2.0
//
// Platform TLS only: SSLContext "TLSv1.3" from the default provider (platform Conscrypt), the TLS exporter from the
// public android.net.ssl.SSLSockets.exportKeyingMaterial (API 31). No hidden API, no bundled Conscrypt, no
// BouncyCastle, no libadb-android, no LGPL code. adbd's server certificate cannot identify adbd (design 3.4): every
// new connection must pass the provenance check (4.4) before any operation is sent.
package cc.nasz.minion.devicecontrol

import android.annotation.SuppressLint
import android.net.ssl.SSLSockets
import android.os.Build
import java.io.IOException
import java.net.ConnectException
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.Socket
import java.net.SocketTimeoutException
import java.security.Principal
import java.security.PrivateKey
import java.security.SecureRandom
import java.security.cert.X509Certificate
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLException
import javax.net.ssl.SSLSocket
import javax.net.ssl.X509ExtendedKeyManager
import javax.net.ssl.X509TrustManager

/** A failure with a stable reason code (bridge protocol `error.code`). Never carries a secret. */
class BridgeException(val code: String, message: String) : IOException(message)

class AdbBridge(private val key: AdbKey, private val provenance: ProvenanceRegistry = ProvenanceRegistry.shared) {
    private val loopback: InetAddress = InetAddress.getByAddress("localhost", byteArrayOf(127, 0, 0, 1))

    init {
        require(Build.VERSION.SDK_INT >= 31) { "needs Android 12 (API 31) for SSLSockets.exportKeyingMaterial" }
    }

    /** Pairs with the Wireless debugging pairing port on 127.0.0.1. [code] is wiped whatever happens. */
    fun pair(port: Int, code: CharArray) {
        try {
            if (!EndpointChooser.validPort(port)) throw BridgeException("bad_request", "Enter the pairing port shown in Settings.")
            val tls = tlsSocket(dial(port, 15_000), port)
            tls.use { s ->
                s.startHandshake()
                if (!SSLSockets.isSupportedSocket(s)) throw BridgeException("internal", "platform TLS socket has no exporter")
                val exported = SSLSockets.exportKeyingMaterial(s, PairingPacket.EXPORTER_LABEL, null, PairingPacket.EXPORTER_SIZE)
                    ?: throw BridgeException("internal", "TLS exporter returned nothing")
                val codeBytes = ByteArray(code.size) { code[it].code.toByte() } // ASCII digits only (keypad)
                code.fill('\u0000')
                val password = codeBytes + exported
                codeBytes.fill(0); exported.fill(0)
                val out = s.outputStream
                val inp = s.inputStream
                Spake2.alice().use { spake ->
                    val ours = spake.generateMessage(password) // wipes password
                    out.write(PairingPacket.frame(PairingPacket.TYPE_SPAKE2_MSG, ours)); out.flush()
                    val theirs = PairingPacket.read(inp, PairingPacket.TYPE_SPAKE2_MSG)
                    val keyMaterial = spake.processMessage(theirs)
                    val cipher = PairingCipher(keyMaterial)
                    keyMaterial.fill(0)
                    try {
                        val info = PairingPacket.peerInfo(AdbPubkey.encodeWithName(key.publicKey, AdbKeys.NAME))
                        out.write(PairingPacket.frame(PairingPacket.TYPE_PEER_INFO, cipher.encrypt(info))); out.flush()
                        val sealed = PairingPacket.read(inp, PairingPacket.TYPE_PEER_INFO)
                        cipher.decrypt(sealed) // AEADBadTagException with a wrong code
                    } finally { cipher.wipe() }
                }
            }
        } catch (e: BridgeException) {
            throw e
        } catch (_: ConnectException) {
            throw BridgeException("pairing_closed", "Nothing answers on the pairing port. Open the pairing screen again.")
        } catch (_: Exception) {
            // No exception text is passed on: messages come only from this class and never contain the code.
            throw BridgeException("code_rejected", CODE_REJECTED)
        } finally {
            code.fill('\u0000')
        }
    }

    /**
     * Connects to the connection port on 127.0.0.1: CNXN -> STLS -> TLS 1.3 with the client key -> CNXN, then the
     * provenance check. Returns a verified, started connection or throws a [BridgeException] with the reason.
     */
    fun connect(port: Int): AdbConnection {
        val raw = try { dial(port, 5_000) } catch (_: IOException) {
            throw BridgeException("needs_wifi", "Nothing answers on 127.0.0.1:$port. Wireless debugging is off or the port changed.")
        }
        var conn: AdbConnection? = null
        try {
            val rawOut = raw.getOutputStream()
            rawOut.write(AdbMessage(AdbMessage.A_CNXN, AdbMessage.VERSION, AdbMessage.MAX_DATA, BANNER).encode())
            rawOut.flush()
            val first = try { AdbMessage.read(raw.getInputStream()) } catch (_: IOException) {
                throw BridgeException("endpoint_unverified", "The endpoint on port $port does not speak adb.")
            }
            if (first.command != AdbMessage.A_STLS) {
                throw BridgeException("endpoint_unverified", "The endpoint on port $port is not Wireless debugging (no TLS).")
            }
            rawOut.write(AdbMessage(AdbMessage.A_STLS, AdbMessage.STLS_VERSION, 0).encode()); rawOut.flush()
            val tls = tlsSocket(raw, port)
            val cnxn = try {
                tls.startHandshake()
                AdbMessage.read(tls.inputStream)
            } catch (_: SocketTimeoutException) {
                throw BridgeException("endpoint_unverified", "The endpoint on port $port did not finish the adb handshake.")
            } catch (_: SSLException) {
                throw BridgeException("not_paired", NOT_PAIRED)
            } catch (_: IOException) {
                throw BridgeException("not_paired", NOT_PAIRED)
            }
            if (cnxn.command != AdbMessage.A_CNXN) throw BridgeException("endpoint_unverified", "Unexpected adb handshake on port $port.")
            tls.soTimeout = 0
            val c = AdbConnection(tls.inputStream, tls.outputStream) { runCatching { tls.close() }; runCatching { raw.close() } }
            conn = c.start()
            verify(c)
            return c
        } catch (e: Exception) {
            conn?.close()
            runCatching { raw.close() }
            if (e is BridgeException) throw e
            throw BridgeException("endpoint_unverified", "The endpoint on port $port failed the adb handshake.")
        }
    }

    /** Design 4.4: the peer must make the provider call as uid 2000 with our nonce within 5 s and exit 0. */
    private fun verify(conn: AdbConnection) {
        val nonce = provenance.issue()
        val exit = try {
            AdbOps.shell(conn, provenance.command(nonce), 5_000) { _, _ -> }
        } catch (_: IOException) {
            -1
        }
        val proven = provenance.await(nonce, if (exit == 0) 5_000 else 1)
        if (exit != 0 || !proven) {
            throw BridgeException("endpoint_unverified", "The endpoint could not prove it is this device's adbd (provenance check failed).")
        }
    }

    private fun dial(port: Int, soTimeout: Int): Socket {
        require(EndpointChooser.validPort(port)) { "bad port" }
        val s = Socket()
        try {
            s.connect(InetSocketAddress(loopback, port), 5_000)
            s.soTimeout = soTimeout
            s.tcpNoDelay = true
        } catch (e: IOException) {
            runCatching { s.close() }
            throw e
        }
        return s
    }

    private fun tlsSocket(base: Socket, port: Int): SSLSocket {
        val ctx = SSLContext.getInstance("TLSv1.3")
        ctx.init(arrayOf(keyManager()), arrayOf(anyServerCertificate()), SecureRandom())
        val s = ctx.socketFactory.createSocket(base, "127.0.0.1", port, true) as SSLSocket
        s.enabledProtocols = arrayOf("TLSv1.3")
        return s
    }

    private fun keyManager() = object : X509ExtendedKeyManager() {
        override fun chooseClientAlias(keyType: Array<out String>?, issuers: Array<out Principal>?, socket: Socket?) =
            if (keyType?.contains("RSA") == true) "adb" else null
        override fun getCertificateChain(alias: String?) = if (alias == "adb") arrayOf(key.certificate) else null
        override fun getPrivateKey(alias: String?): PrivateKey? = if (alias == "adb") key.privateKey else null
        override fun getClientAliases(keyType: String?, issuers: Array<out Principal>?) = arrayOf("adb")
        override fun getServerAliases(keyType: String?, issuers: Array<out Principal>?) = null
        override fun chooseServerAlias(keyType: String?, issuers: Array<out Principal>?, socket: Socket?) = null
    }

    // adbd's certificate is generated at runtime and identifies nothing (design 3.4). Trust comes from the
    // loopback-only dial plus the provenance check of every connection, not from the certificate.
    @SuppressLint("CustomX509TrustManager", "TrustAllX509TrustManager")
    private fun anyServerCertificate() = object : X509TrustManager {
        override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
        override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
        override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
    }

    companion object {
        val BANNER = "host::features=shell_v2,cmd\u0000".toByteArray(Charsets.UTF_8)
        const val CODE_REJECTED = "Code not accepted. Open the pairing screen again for a new code."
        const val NOT_PAIRED = "Authorization ended in Android. Pair again in the Minion app."
    }
}
