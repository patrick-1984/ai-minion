// Device control: Kotlin side of the BoringSSL SPAKE2 JNI shim (libminion_spake2.so).
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import java.io.Closeable

/** SPAKE2 (spake25519) context as used by adb pairing. Not thread-safe; one use only. */
class Spake2 private constructor(private var handle: Long) : Closeable {
    /** Our message for the given password (code bytes + 64 TLS exporter bytes). Wipes [password]. */
    fun generateMessage(password: ByteArray): ByteArray = nativeGenerateMsg(live(), password)

    /** Processes the peer message and returns the 64-byte shared key material. */
    fun processMessage(theirMessage: ByteArray): ByteArray = nativeProcessMsg(live(), theirMessage)

    private fun live(): Long = handle.also { check(it != 0L) { "SPAKE2 context closed" } }

    override fun close() {
        if (handle != 0L) nativeFree(handle)
        handle = 0L
    }

    companion object {
        val CLIENT_NAME = "adb pair client\u0000".toByteArray(Charsets.UTF_8)
        val SERVER_NAME = "adb pair server\u0000".toByteArray(Charsets.UTF_8)

        @Volatile private var loaded = false

        /** Loads the shim. Host tests set -Dminion.spake2.lib=/abs/path/libminion_spake2.so. */
        @Synchronized
        fun load() {
            if (loaded) return
            val path = System.getProperty("minion.spake2.lib")
            if (path.isNullOrEmpty()) System.loadLibrary("minion_spake2") else System.load(path)
            loaded = true
        }

        fun alice(): Spake2 = load().let { Spake2(nativeNew(0, CLIENT_NAME, SERVER_NAME)) }

        /** Server role, for tests only. */
        fun bob(): Spake2 = load().let { Spake2(nativeNew(1, SERVER_NAME, CLIENT_NAME)) }

        @JvmStatic private external fun nativeNew(role: Int, myName: ByteArray, theirName: ByteArray): Long
        @JvmStatic private external fun nativeGenerateMsg(handle: Long, password: ByteArray): ByteArray
        @JvmStatic private external fun nativeProcessMsg(handle: Long, theirMessage: ByteArray): ByteArray
        @JvmStatic private external fun nativeFree(handle: Long)
    }
}
