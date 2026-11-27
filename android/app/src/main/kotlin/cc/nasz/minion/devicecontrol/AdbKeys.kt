// Device control: the adb client key. AndroidKeyStore RSA-2048 (non-exportable) or, if Keystore cannot make
// one, a software key in an app-private file.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyInfo
import android.security.keystore.KeyProperties
import android.system.Os
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

class AdbKey(val inKeystore: Boolean, val privateKey: PrivateKey, val certificate: X509Certificate) {
    val publicKey: RSAPublicKey get() = certificate.publicKey as RSAPublicKey

    /** "android-keystore (TEE|STRONGBOX|SOFTWARE|...)" or "app-file" (design 4.7, G12). */
    fun storageDescription(): String {
        if (!inKeystore) return "app-file"
        val level = runCatching {
            val info = KeyFactory.getInstance(privateKey.algorithm, "AndroidKeyStore").getKeySpec(privateKey, KeyInfo::class.java)
            if (android.os.Build.VERSION.SDK_INT >= 31) when (info.securityLevel) {
                KeyProperties.SECURITY_LEVEL_TRUSTED_ENVIRONMENT -> "TEE"
                KeyProperties.SECURITY_LEVEL_STRONGBOX -> "STRONGBOX"
                KeyProperties.SECURITY_LEVEL_SOFTWARE -> "SOFTWARE"
                KeyProperties.SECURITY_LEVEL_UNKNOWN_SECURE -> "UNKNOWN_SECURE"
                else -> "UNKNOWN"
            } else if (@Suppress("DEPRECATION") info.isInsideSecureHardware) "secure-hardware" else "software"
        }.getOrDefault("unknown")
        return "android-keystore ($level)"
    }
}

object AdbKeys {
    const val SUBJECT = "CN=minion@cc.nasz.minion"
    const val NAME = "minion@cc.nasz.minion"
    const val ALIAS = "minion-adb"
    const val FILE_WARNING = "Key stored in an app-private file. Every terminal of this Worker could read it."

    /** Keystore key if possible, else the file key in [stateDir]/adb (0700 dir, 0600 files). */
    fun load(stateDir: File): AdbKey = runCatching { keystoreKey() }.getOrElse { fileKey(File(stateDir, "adb")) }

    /** Removes the local key (Keystore entry and file); a new pairing makes a new one. */
    fun delete(stateDir: File) {
        runCatching { KeyStore.getInstance("AndroidKeyStore").apply { load(null) }.deleteEntry(ALIAS) }
        File(stateDir, "adb/adbkey").delete()
        File(stateDir, "adb/adbkey.crt").delete()
    }

    /**
     * Key spec per K0 (desk) Q2: platform Conscrypt signs TLS 1.3 rsa_pss_rsae_sha256 by padding in BoringSSL and
     * calling Cipher "RSA/ECB/NoPadding" with the private key, which AndroidKeyStore maps to a raw SIGN with
     * KM_PAD_NONE + KM_DIGEST_NONE. So the key needs DIGEST_NONE and ENCRYPTION_PADDING_NONE; purpose stays SIGN.
     */
    fun keySpec(): KeyGenParameterSpec = KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
        .setAlgorithmParameterSpec(RSAKeyGenParameterSpec(2048, RSAKeyGenParameterSpec.F4))
        .setCertificateSubject(X500Principal(SUBJECT))
        .setCertificateSerialNumber(BigInteger.ONE)
        .setCertificateNotBefore(Date(System.currentTimeMillis() - 86_400_000L))
        .setCertificateNotAfter(Date(System.currentTimeMillis() + 20L * 365 * 86_400_000L))
        .setSignaturePaddings(KeyProperties.SIGNATURE_PADDING_RSA_PSS, KeyProperties.SIGNATURE_PADDING_RSA_PKCS1)
        .setDigests(KeyProperties.DIGEST_SHA256, KeyProperties.DIGEST_NONE)
        .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
        .setRandomizedEncryptionRequired(false)
        .build()

    private fun keystoreKey(): AdbKey {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        if (!ks.containsAlias(ALIAS)) {
            KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_RSA, "AndroidKeyStore").apply {
                initialize(keySpec())
                generateKeyPair()
            }
        }
        val entry = ks.getEntry(ALIAS, null) as KeyStore.PrivateKeyEntry
        return AdbKey(true, entry.privateKey, entry.certificate as X509Certificate)
    }

    private fun fileKey(dir: File): AdbKey {
        check(dir.isDirectory || dir.mkdirs())
        Os.chmod(dir.path, 448) // 0700
        val keyFile = File(dir, "adbkey")
        val certFile = File(dir, "adbkey.crt")
        if (!keyFile.exists() || !certFile.exists()) {
            val kp = KeyPairGenerator.getInstance("RSA").apply { initialize(RSAKeyGenParameterSpec(2048, RSAKeyGenParameterSpec.F4)) }
                .generateKeyPair()
            writePrivate(keyFile, kp.private.encoded)
            writePrivate(certFile, SelfSignedCert.build(kp.private, kp.public.encoded, SUBJECT))
        }
        val priv = KeyFactory.getInstance("RSA").generatePrivate(PKCS8EncodedKeySpec(keyFile.readBytes()))
        val cert = certFile.inputStream().use { CertificateFactory.getInstance("X.509").generateCertificate(it) as X509Certificate }
        return AdbKey(false, priv, cert)
    }

    private fun writePrivate(f: File, bytes: ByteArray) {
        val tmp = File(f.parentFile, f.name + ".tmp")
        tmp.outputStream().use { it.write(bytes) }
        Os.chmod(tmp.path, 384) // 0600
        check(tmp.renameTo(f))
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
