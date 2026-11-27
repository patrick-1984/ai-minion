// Device control: endpoint provenance (design 4.4). After TLS, the bridge asks the peer to run
// `content call --uri content://cc.nasz.minion.adbprobe --method prove --arg <nonce>`. Only a real adbd shell
// (uid 2000) can make that binder call; a fake loopback server cannot. Fails closed.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import android.content.ContentProvider
import android.content.ContentValues
import android.database.Cursor
import android.net.Uri
import android.os.Binder
import android.os.Bundle
import java.security.SecureRandom
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicLong

/** Pending one-time nonces. Pure JVM (unit-tested); the provider below is its only Android caller. */
class ProvenanceRegistry(private val random: SecureRandom = SecureRandom()) {
    private val pending = ConcurrentHashMap<String, CountDownLatch>()
    val denied = AtomicLong()

    /** A fresh 128-bit nonce (32 hex characters), valid until [await] returns. */
    fun issue(): String {
        val bytes = ByteArray(16).also(random::nextBytes)
        val nonce = bytes.joinToString("") { "%02x".format(it) }
        pending[nonce] = CountDownLatch(1)
        return nonce
    }

    /** Called from the provider. True only for the shell uid and a pending nonce; anything else is counted. */
    fun prove(callingUid: Int, nonce: String?): Boolean {
        val latch = if (callingUid == SHELL_UID && nonce != null) pending[nonce] else null
        if (latch == null) { denied.incrementAndGet(); return false }
        latch.countDown()
        return true
    }

    /** Waits until the nonce was proven by uid 2000; the nonce is gone afterwards either way. */
    fun await(nonce: String, timeoutMs: Long): Boolean {
        val latch = pending[nonce] ?: return false
        return try { latch.await(timeoutMs, TimeUnit.MILLISECONDS) } finally { pending.remove(nonce) }
    }

    fun command(nonce: String): String =
        "content call --uri content://$AUTHORITY --method $METHOD --arg $nonce"

    companion object {
        const val SHELL_UID = 2000
        const val AUTHORITY = "cc.nasz.minion.adbprobe"
        const val METHOD = "prove"
        val shared = ProvenanceRegistry()
    }
}

/** Exported (shell must reach it) but without data and with a single method; declared only in flag builds. */
class ProvenanceProvider : ContentProvider() {
    override fun onCreate() = true

    override fun call(method: String, arg: String?, extras: Bundle?): Bundle? {
        if (method != ProvenanceRegistry.METHOD || !ProvenanceRegistry.shared.prove(Binder.getCallingUid(), arg)) {
            if (method != ProvenanceRegistry.METHOD) ProvenanceRegistry.shared.denied.incrementAndGet()
            throw SecurityException("not allowed")
        }
        return null
    }

    override fun query(uri: Uri, projection: Array<out String>?, selection: String?, selectionArgs: Array<out String>?, sortOrder: String?): Cursor? = refuse()
    override fun getType(uri: Uri): String? = null
    override fun insert(uri: Uri, values: ContentValues?): Uri? = refuse()
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?): Int = refuse()
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?): Int = refuse()

    private fun refuse(): Nothing {
        ProvenanceRegistry.shared.denied.incrementAndGet()
        throw SecurityException("not allowed")
    }
}
