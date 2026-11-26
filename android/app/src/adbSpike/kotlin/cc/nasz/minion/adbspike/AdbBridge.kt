// Spike K0: smallest adb client: pair (code, port) and run one shell command over TLS on 127.0.0.1.
// SPDX-License-Identifier: Apache-2.0
//
// Platform TLS only: SSLContext "TLSv1.3" from the default provider (platform Conscrypt), the TLS exporter from
// the public android.net.ssl.SSLSockets.exportKeyingMaterial (API 31). No hidden API, no bundled Conscrypt.
// Spike limits (K1 must fix): trusts any adbd server certificate (design 4.4 provenance check comes later),
// one stream at a time, no shell v2 exit codes.
package cc.nasz.minion.adbspike

import android.net.ssl.SSLSockets
import android.os.Build
import java.io.ByteArrayOutputStream
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.InetSocketAddress
import java.net.Socket
import java.security.MessageDigest
import java.security.Principal
import java.security.PrivateKey
import java.security.SecureRandom
import java.security.cert.X509Certificate
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSocket
import javax.net.ssl.X509ExtendedKeyManager
import javax.net.ssl.X509TrustManager

class AdbBridge(private val key: AdbKey, private val log: (String) -> Unit) {
    private val host = "127.0.0.1"

    init {
        require(Build.VERSION.SDK_INT >= 31) { "needs Android 12 (API 31) for SSLSockets.exportKeyingMaterial" }
    }

    /** Pairs with the Wireless debugging pairing port. [code] is wiped. */
    fun pair(port: Int, code: CharArray) {
        val tls = tlsSocket(Socket().apply { connect(InetSocketAddress(host, port), 5000); soTimeout = 15000; tcpNoDelay = true }, port)
        tls.use { s ->
            s.startHandshake()
            describe("pairing", s)
            check(SSLSockets.isSupportedSocket(s)) { "platform SSLSocket not supported by SSLSockets" }
            val exported = SSLSockets.exportKeyingMaterial(s, PairingPacket.EXPORTER_LABEL, null, PairingPacket.EXPORTER_SIZE)
                ?: throw IOException("SSLSockets.exportKeyingMaterial returned null")
            log("exporter: ${exported.size} bytes from SSLSockets.exportKeyingMaterial")
            val codeBytes = String(code).toByteArray(Charsets.UTF_8)
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
                    val info = PairingPacket.peerInfo(AdbPubkey.encodeWithName(key.publicKey, "minion@cc.nasz.minion"))
                    out.write(PairingPacket.frame(PairingPacket.TYPE_PEER_INFO, cipher.encrypt(info))); out.flush()
                    val sealed = PairingPacket.read(inp, PairingPacket.TYPE_PEER_INFO)
                    val peer = try { cipher.decrypt(sealed) } catch (e: Exception) {
                        throw IOException("pairing: wrong code (peer info did not decrypt)", e)
                    }
                    check(peer.size == PairingPacket.PEER_INFO_SIZE) { "pairing: peer info size ${peer.size}" }
                    // Type 1 = ADB_DEVICE_GUID. The GUID is a hint only (design 3.4), logged for the report.
                    val guid = String(peer, 1, peer.size - 1, Charsets.UTF_8).substringBefore('\u0000')
                    log("paired: peer info type=${peer[0]} guid=$guid")
                } finally { cipher.wipe() }
            }
        }
    }

    /** Connects to the Wireless debugging connection port and runs `shell:<command>`; returns its output. */
    fun shell(port: Int, command: String): String {
        val raw = Socket().apply { connect(InetSocketAddress(host, port), 5000); soTimeout = 15000; tcpNoDelay = true }
        raw.use {
            val rawOut = raw.getOutputStream()
            rawOut.write(AdbMessage(AdbMessage.A_CNXN, AdbMessage.VERSION, AdbMessage.MAX_DATA, "host::\u0000".toByteArray()).encode())
            rawOut.flush()
            val first = AdbMessage.read(raw.getInputStream())
            log("adbd answered ${AdbMessage.name(first.command)}")
            if (first.command == AdbMessage.A_AUTH) throw IOException("adbd asked for legacy AUTH, not TLS: is this the Wireless debugging port?")
            check(first.command == AdbMessage.A_STLS) { "expected STLS, got ${AdbMessage.name(first.command)}" }
            rawOut.write(AdbMessage(AdbMessage.A_STLS, AdbMessage.STLS_VERSION, 0).encode()); rawOut.flush()
            val tls = tlsSocket(raw, port)
            tls.startHandshake() // proves the Keystore key signed CertificateVerify (adbd checks it against paired keys)
            describe("connect", tls)
            val inp = tls.inputStream
            val out = tls.outputStream
            val cnxn = AdbMessage.read(inp)
            check(cnxn.command == AdbMessage.A_CNXN) { "expected CNXN after TLS, got ${AdbMessage.name(cnxn.command)}" }
            log("device banner: ${String(cnxn.data, Charsets.UTF_8).trimEnd('\u0000').take(200)}")
            return runStream(inp, out, "shell:$command")
        }
    }

    private fun runStream(inp: InputStream, out: OutputStream, service: String): String {
        val localId = 1
        out.write(AdbMessage(AdbMessage.A_OPEN, localId, 0, (service + "\u0000").toByteArray()).encode()); out.flush()
        var remoteId = 0
        val result = ByteArrayOutputStream()
        while (true) {
            val m = AdbMessage.read(inp)
            when (m.command) {
                AdbMessage.A_OKAY -> remoteId = m.arg0
                AdbMessage.A_WRTE -> {
                    result.write(m.data)
                    out.write(AdbMessage(AdbMessage.A_OKAY, localId, m.arg0).encode()); out.flush()
                }
                AdbMessage.A_CLSE -> {
                    if (remoteId != 0) { out.write(AdbMessage(AdbMessage.A_CLSE, localId, remoteId).encode()); out.flush() }
                    return result.toString(Charsets.UTF_8.name())
                }
                else -> log("ignored ${AdbMessage.name(m.command)}")
            }
        }
    }

    private fun tlsSocket(base: Socket, port: Int): SSLSocket {
        val ctx = SSLContext.getInstance("TLSv1.3")
        ctx.init(arrayOf(keyManager()), arrayOf(trustAnyServer()), SecureRandom())
        log("TLS provider: ${ctx.provider.name} ${ctx.provider.version}")
        val s = ctx.socketFactory.createSocket(base, host, port, true) as SSLSocket
        s.enabledProtocols = arrayOf("TLSv1.3")
        return s
    }

    private fun describe(what: String, s: SSLSocket) {
        val sess = s.session
        val server = (sess.peerCertificates.firstOrNull() as? X509Certificate)?.encoded
        val fp = server?.let { MessageDigest.getInstance("SHA-256").digest(it).joinToString("") { b -> "%02x".format(b) } }
        log("$what TLS ok: ${sess.protocol} ${sess.cipherSuite}; client key ${key.storageDescription()}; server cert sha256=$fp")
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

    // Spike only: adbd's certificate is not pinned here. K1 implements the provenance check of design 4.4.
    @android.annotation.SuppressLint("CustomX509TrustManager", "TrustAllX509TrustManager")
    private fun trustAnyServer() = object : X509TrustManager {
        override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
        override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
        override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
    }
}
