package cc.nasz.minion

import cc.nasz.minion.WorkerStatusLine.Input
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class WorkerStatusLineTest {
    private val running = Input(wanted = true, workerAlive = true, reportAvailable = true)

    @Test
    fun unavailableOrEarlyStatusIsStartingNeverNotConfigured() {
        // Right after a boot: the Worker process is not up, or its status read failed.
        assertEquals(WorkerStatusLine.STARTING, WorkerStatusLine.of(Input(wanted = true)))
        assertEquals(WorkerStatusLine.STARTING, WorkerStatusLine.of(running.copy(reportAvailable = false)))
        assertEquals(WorkerStatusLine.STARTING, WorkerStatusLine.of(running.copy(workerAlive = false, state = "connected")))
        // The same without a network.
        assertEquals(WorkerStatusLine.WAITING_FOR_NETWORK, WorkerStatusLine.of(Input(wanted = true, networkUp = false)))
    }

    @Test
    fun aMissingReportKeepsTheSavedHub() {
        assertEquals("https://hub.example", WorkerStatusLine.hub(null, "https://hub.example"))
        assertEquals("https://hub.example", WorkerStatusLine.hub("", "https://hub.example"))
        assertEquals("https://new.example", WorkerStatusLine.hub("https://new.example", "https://hub.example"))
        // Only when the app never saved one is there no Hub.
        assertEquals("", WorkerStatusLine.hub(null, null))
    }

    @Test
    fun connectedAndRecentlyVerifiedIsConnected() {
        assertEquals(WorkerStatusLine.CONNECTED, WorkerStatusLine.of(running.copy(state = "connected", verified = true, heardAgeSeconds = 3)))
        // An older Worker that does not report the link's age.
        assertEquals(WorkerStatusLine.CONNECTED, WorkerStatusLine.of(running.copy(state = "connected")))
        assertEquals("Awaiting Hub approval", WorkerStatusLine.of(running.copy(state = "connected", verified = true, approvalPending = true)))
    }

    @Test
    fun connectedButNotVerifiedRecentlyIsReconnecting() {
        assertEquals(WorkerStatusLine.RECONNECTING, WorkerStatusLine.of(running.copy(state = "connected", verified = false, heardAgeSeconds = 600)))
        assertEquals(WorkerStatusLine.RECONNECTING, WorkerStatusLine.of(running.copy(state = "connected", heardAgeSeconds = WorkerStatusLine.FRESH_SECONDS + 1)))
        assertEquals(WorkerStatusLine.CONNECTED, WorkerStatusLine.of(running.copy(state = "connected", heardAgeSeconds = WorkerStatusLine.FRESH_SECONDS)))
    }

    @Test
    fun otherStates() {
        assertEquals(WorkerStatusLine.CONNECTING, WorkerStatusLine.of(running.copy(state = "backoff")))
        assertEquals(WorkerStatusLine.WAITING_FOR_NETWORK, WorkerStatusLine.of(running.copy(state = "backoff", networkUp = false)))
        assertEquals("Stopped", WorkerStatusLine.of(Input()))
        assertEquals("Paused", WorkerStatusLine.of(running.copy(paused = true, state = "idle")))
        assertEquals("Setting up", WorkerStatusLine.of(Input(busy = true)))
    }

    @Test
    fun reconnectRequestsAreRateLimited() {
        val n = ReconnectNudge(5_000)
        assertTrue(n.take(1_000))
        assertFalse(n.take(2_000))
        assertFalse(n.take(5_999))
        assertTrue(n.take(6_000))
    }
}
