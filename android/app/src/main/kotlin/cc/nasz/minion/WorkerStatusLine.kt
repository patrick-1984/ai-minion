package cc.nasz.minion

/**
 * The status line of the app, from what the service knows. Pure Kotlin, so the
 * mapping is unit tested without Android.
 *
 * The Worker's own report (`worker status --json`) is the only source of the
 * live link state. A report that is missing (the Worker is still starting,
 * right after a boot, or its config is not readable yet) never means "no Hub":
 * whether a Hub is set up comes from the app's own saved state as well.
 */
internal object WorkerStatusLine {
    const val STARTING = "Starting..."
    const val WAITING_FOR_NETWORK = "Waiting for the network..."
    const val CONNECTED = "Connected"
    const val RECONNECTING = "Reconnecting..."
    const val CONNECTING = "Connecting to the Hub"

    /**
     * How long the Hub may be silent on a connected link before the app stops
     * calling it Connected, when the Worker does not say it itself (verified).
     * A healthy link hears the Hub at least every 15 s.
     */
    const val FRESH_SECONDS = 45L

    data class Input(
        val busy: Boolean = false,
        val wanted: Boolean = false,
        val paused: Boolean = false,
        val identityAlarm: Boolean = false,
        val pairing: Boolean = false,
        val usageRequired: Boolean = false,
        val workerAlive: Boolean = false,
        /** A report of the running Worker was read. */
        val reportAvailable: Boolean = false,
        /** connection.state of the report ("" when unknown). */
        val state: String = "",
        val approvalPending: Boolean = false,
        /** connection.verified; null from a Worker that does not report it. */
        val verified: Boolean? = null,
        /** connection.last_heard_age_s; null when not reported. */
        val heardAgeSeconds: Long? = null,
        /** The device has a network (null: not known yet). */
        val networkUp: Boolean? = null,
    )

    fun of(i: Input): String = when {
        i.busy -> "Setting up"
        !i.wanted -> "Stopped"
        i.paused -> "Paused"
        i.identityAlarm -> "Hub identity changed"
        i.pairing -> "Ready to pair"
        i.usageRequired -> "Usage acceptance required"
        !i.workerAlive || !i.reportAvailable -> if (i.networkUp == false) WAITING_FOR_NETWORK else STARTING
        i.state == "connected" -> when {
            !linkFresh(i) -> RECONNECTING
            i.approvalPending -> "Awaiting Hub approval"
            else -> CONNECTED
        }
        i.networkUp == false -> WAITING_FOR_NETWORK
        else -> CONNECTING
    }

    private fun linkFresh(i: Input): Boolean = when {
        i.verified != null -> i.verified
        i.heardAgeSeconds != null -> i.heardAgeSeconds <= FRESH_SECONDS
        else -> true // an older Worker: its connected state is all there is
    }

    /**
     * Whether a Hub is set up: the Worker's report says so, or the app saved
     * so earlier and the report does not say otherwise yet. Only the app's
     * own saved state can make a missing report "configured".
     */
    fun hub(reportHub: String?, savedHub: String?): String =
        reportHub?.takeIf { it.isNotBlank() } ?: savedHub.orEmpty()
}

/**
 * Rate limit of the "reconnect now" requests that network changes cause: a
 * network that flaps sends many callbacks, and one request per window is enough
 * (the Worker probes a working link and keeps it).
 */
internal class ReconnectNudge(private val minGapMillis: Long = 5_000) {
    private var last = Long.MIN_VALUE

    /** Whether a request should be sent now; records it when so. */
    @Synchronized
    fun take(nowMillis: Long): Boolean {
        if (last != Long.MIN_VALUE && nowMillis - last < minGapMillis) return false
        last = nowMillis
        return true
    }
}
