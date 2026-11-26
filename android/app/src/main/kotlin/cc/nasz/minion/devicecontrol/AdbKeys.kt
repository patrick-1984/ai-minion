// Spike K0: adb client key in AndroidKeyStore (two variants) or in an app-private file (fallback).
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.adbspike

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyInfo
import android.security.keystore.KeyProperties
import java.io.ByteArrayOutputStream
import java.io.File
import java.math.BigInteger
import java.security.KeyFactory
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey
import java.security.Signature
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate
import java.security.interfaces.RSAPublicKey
import java.security.spec.PKCS8EncodedKeySpec
import java.security.spec.RSAKeyGenParameterSpec
import java.util.Date
import javax.security.auth.x500.X500Principal

enum class KeyMode(val alias: String) {
    /**
     * Keystore RSA-2048 that also allows raw RSA (padding NONE, digest NONE). Needed because platform Conscrypt
     * signs TLS 1.3 rsa_pss_rsae_sha256 by padding in BoringSSL and calling Cipher "RSA/ECB/NoPadding"
     * (private-key encrypt) on the Keystore key (CryptoUpcalls.rsaSignDigestWithPrivateKey).
     */
    KEYSTORE_RAW("minion-adb-spike-raw"),

    /** Keystore RSA-2048 restricted to PSS/PKCS1 with SHA-256 (design v2 wording). Expected to FAIL the TLS 1.3 handshake. */
    KEYSTORE_PSS_ONLY("minion-adb-spike-pss"),

    /** Software RSA-2048 in noBackupFilesDir/adbspike/adbkey (0600): the design's fallback. */
    APP_FILE("app-file"),
}

class AdbKey(val mode: KeyMode, val privateKey: PrivateKey, val certificate: X509Certificate) {
    val publicKey: RSAPublicKey get() = certificate.publicKey as RSAPublicKey

    /** "android-keystore (TEE|STRONGBOX|SOFTWARE|...)" or "app-file". */
    fun storageDescription(): String {
        if (mode == KeyMode.APP_FILE) return "app-file"
        val info = KeyFactory.getInstance(privateKey.algorithm, "AndroidKeyStore").getKeySpec(privateKey, KeyInfo::class.java)
        val level = if (android.os.Build.VERSION.SDK_INT >= 31) when (info.securityLevel) {
            KeyProperties.SECURITY_LEVEL_TRUSTED_ENVIRONMENT -> "TEE"
            KeyProperties.SECURITY_LEVEL_STRONGBOX -> "STRONGBOX"
            KeyProperties.SECURITY_LEVEL_SOFTWARE -> "SOFTWARE"
            KeyProperties.SECURITY_LEVEL_UNKNOWN_SECURE -> "UNKNOWN_SECURE"
            else -> "UNKNOWN(${info.securityLevel})"
        } else if (@Suppress("DEPRECATION") info.isInsideSecureHardware) "secure-hardware" else "software"
        return "android-keystore ($level)"
    }
}

object AdbKeys {
    const val SUBJECT = "CN=minion@cc.nasz.minion"

    fun load(context: Context, mode: KeyMode): AdbKey = when (mode) {
        KeyMode.APP_FILE -> fileKey(context)
        else -> keystoreKey(mode)
    }

    fun delete(context: Context, mode: KeyMode) {
        if (mode == KeyMode.APP_FILE) File(context.noBackupFilesDir, "adbspike").deleteRecursively()
        else KeyStore.getInstance("AndroidKeyStore").apply { load(null) }.deleteEntry(mode.alias)
    }

    private fun keystoreKey(mode: KeyMode): AdbKey {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        if (!ks.containsAlias(mode.alias)) {
            val b = KeyGenParameterSpec.Builder(mode.alias, KeyProperties.PURPOSE_SIGN)
                .setAlgorithmParameterSpec(RSAKeyGenParameterSpec(2048, RSAKeyGenParameterSpec.F4))
                .setCertificateSubject(X500Principal(SUBJECT))
                .setCertificateSerialNumber(BigInteger.ONE)
                .setCertificateNotBefore(Date(System.currentTimeMillis() - 86_400_000L))
                .setCertificateNotAfter(Date(System.currentTimeMillis() + 20L * 365 * 86_400_000L))
                .setSignaturePaddings(KeyProperties.SIGNATURE_PADDING_RSA_PSS, KeyProperties.SIGNATURE_PADDING_RSA_PKCS1)
            if (mode == KeyMode.KEYSTORE_RAW) {
                // KM_PAD_NONE + KM_DIGEST_NONE for the raw private-key operation Conscrypt performs.
                b.setDigests(KeyProperties.DIGEST_SHA256, KeyProperties.DIGEST_NONE)
                    .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                    .setRandomizedEncryptionRequired(false)
            } else {
                b.setDigests(KeyProperties.DIGEST_SHA256)
            }
            KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_RSA, "AndroidKeyStore").apply {
                initialize(b.build())
                generateKeyPair()
            }
        }
        val entry = ks.getEntry(mode.alias, null) as KeyStore.PrivateKeyEntry
        return AdbKey(mode, entry.privateKey, entry.certificate as X509Certificate)
    }

    private fun fileKey(context: Context): AdbKey {
        val dir = File(context.noBackupFilesDir, "adbspike").apply { mkdirs(); setReadable(false, false); setReadable(true, true) }
        val keyFile = File(dir, "adbkey")
        val certFile = File(dir, "adbkey.crt")
        if (!keyFile.exists() || !certFile.exists()) {
            val kp = KeyPairGenerator.getInstance("RSA").apply { initialize(RSAKeyGenParameterSpec(2048, RSAKeyGenParameterSpec.F4)) }
                .generateKeyPair()
            val cert = SelfSignedCert.build(kp.private, kp.public.encoded, SUBJECT)
            writePrivate(keyFile, kp.private.encoded)
            writePrivate(certFile, cert)
        }
        val priv = KeyFactory.getInstance("RSA").generatePrivate(PKCS8EncodedKeySpec(keyFile.readBytes()))
        val cert = CertificateFactory.getInstance("X.509").generateCertificate(certFile.inputStream()) as X509Certificate
        return AdbKey(KeyMode.APP_FILE, priv, cert)
    }

    private fun writePrivate(f: File, bytes: ByteArray) {
        f.writeBytes(bytes)
        f.setReadable(false, false); f.setReadable(true, true)
        f.setWritable(false, false); f.setWritable(true, true)
    }
}

/** Minimal DER writer for a v3 self-signed sha256WithRSAEncryption certificate (fallback key only). */
object SelfSignedCert {
    private val SHA256_RSA = byteArrayOf(0x06, 0x09, 0x2a, 0x86.toByte(), 0x48, 0x86.toByte(), 0xf7.toByte(), 0x0d, 0x01, 0x01, 0x0b)
    private val CN = byteArrayOf(0x06, 0x03, 0x55, 0x04, 0x03)

    fun build(key: PrivateKey, spki: ByteArray, subject: String): ByteArray {
        val algId = seq(SHA256_RSA + byteArrayOf(0x05, 0x00))
        val name = seq(tlv(0x31, seq(CN + tlv(0x0c, subject.removePrefix("CN=").toByteArray(Charsets.UTF_8)))))
        val now = System.currentTimeMillis()
        val validity = seq(utc(now - 86_400_000L) + utc(now + 20L * 365 * 86_400_000L))
        val tbs = seq(tlv(0xa0, tlv(0x02, byteArrayOf(2))) + tlv(0x02, byteArrayOf(1)) + algId + name + validity + name + spki)
        val sig = Signature.getInstance("SHA256withRSA").run { initSign(key); update(tbs); sign() }
        return seq(tbs + algId + tlv(0x03, byteArrayOf(0) + sig))
    }

    private fun utc(ms: Long): ByteArray {
        val f = java.text.SimpleDateFormat("yyMMddHHmmss'Z'", java.util.Locale.US).apply { timeZone = java.util.TimeZone.getTimeZone("UTC") }
        return tlv(0x17, f.format(Date(ms)).toByteArray(Charsets.US_ASCII))
    }

    private fun seq(content: ByteArray) = tlv(0x30, content)

    fun tlv(tag: Int, content: ByteArray): ByteArray {
        val out = ByteArrayOutputStream()
        out.write(tag)
        val n = content.size
        when {
            n < 0x80 -> out.write(n)
            n < 0x100 -> { out.write(0x81); out.write(n) }
            n < 0x10000 -> { out.write(0x82); out.write(n shr 8); out.write(n and 0xff) }
            else -> { out.write(0x83); out.write(n shr 16); out.write((n shr 8) and 0xff); out.write(n and 0xff) }
        }
        out.write(content)
        return out.toByteArray()
    }
}
