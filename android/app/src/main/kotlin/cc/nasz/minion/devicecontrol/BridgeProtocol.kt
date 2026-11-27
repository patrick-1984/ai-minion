// Device control: frames of the Worker -> app bridge socket (r/minion/adb-bridge.sock). Pure JVM; unit-tested.
// The full protocol is in android/DEVICE-CONTROL.md.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import java.io.DataInputStream
import java.io.DataOutputStream
import java.io.IOException
import java.security.MessageDigest
import java.security.SecureRandom

/** Frame = u32 big-endian n (1..MAX+1) | u8 channel | n-1 payload bytes. Channel 0 = JSON, 1 = stdout, 2 = stderr. */
object BridgeFrames {
    const val CONTROL = 0
    const val STDOUT = 1
    const val STDERR = 2
    const val MAX_PAYLOAD = 1 shl 20

    fun write(out: DataOutputStream, channel: Int, payload: ByteArray) {
        require(channel in 0..2 && payload.size <= MAX_PAYLOAD)
        synchronized(out) {
            out.writeInt(payload.size + 1)
            out.writeByte(channel)
            out.write(payload)
            out.flush()
        }
    }

    /** Large outputs are split, so no frame is bigger than [MAX_PAYLOAD]. */
    fun writeData(out: DataOutputStream, channel: Int, data: ByteArray) {
        var pos = 0
        while (pos < data.size) {
            val n = minOf(MAX_PAYLOAD, data.size - pos)
            write(out, channel, data.copyOfRange(pos, pos + n))
            pos += n
        }
    }

    fun read(inp: DataInputStream, maxPayload: Int = MAX_PAYLOAD): Pair<Int, ByteArray> {
        val n = inp.readInt()
        if (n < 1 || n - 1 > maxPayload) throw IOException("bridge: bad frame size")
        val channel = inp.readUnsignedByte()
        if (channel > 2) throw IOException("bridge: bad channel")
        return channel to ByteArray(n - 1).also { inp.readFully(it) }
    }
}

/** Per-start bridge token (design 2.3): stops accidental use by other same-uid code; not a security boundary. */
object BridgeToken {
    fun generate(random: SecureRandom = SecureRandom()): String =
        ByteArray(32).also(random::nextBytes).joinToString("") { "%02x".format(it) }

    fun matches(expected: String, given: String?): Boolean =
        given != null && MessageDigest.isEqual(expected.toByteArray(Charsets.US_ASCII), given.toByteArray(Charsets.US_ASCII))

    /** The single line the app writes to the Worker's stdin at launch. */
    fun stdinLine(token: String) = "MINION_ADB_BRIDGE_TOKEN=$token\n"
}

/** Limits of design 4.6. */
object OpLimits {
    const val DEFAULT_TIMEOUT_MS = 120_000L
    const val MAX_TIMEOUT_MS = 3_600_000L
    const val SCREENCAP_TIMEOUT_MS = 60_000L
    const val MAX_COMMAND = 64 * 1024
    const val MAX_CONCURRENT = 8

    fun timeout(requested: Long?): Long = when {
        requested == null || requested <= 0 -> DEFAULT_TIMEOUT_MS
        else -> minOf(requested, MAX_TIMEOUT_MS)
    }
}
