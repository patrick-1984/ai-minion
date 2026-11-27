package cc.nasz.minion

import cc.nasz.minion.Autostart.Trigger.APP_OPENED
import cc.nasz.minion.Autostart.Trigger.BOOT
import cc.nasz.minion.Autostart.Trigger.PACKAGE_REPLACED
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class AutostartTest {
    @Test
    fun enabledByDefaultAndExplicitChoiceKept() {
        assertTrue(Autostart.enabled(null)) // fresh install, or never answered
        assertTrue(Autostart.enabled(true))
        assertFalse(Autostart.enabled(false)) // an earlier "No" stays
    }

    @Test
    fun bootStartsWheneverEnabled() {
        for (running in listOf(false, true)) for (stopped in listOf(false, true)) {
            assertTrue(Autostart.shouldStart(BOOT, enabled = true, running = running, stoppedByUser = stopped))
            assertFalse(Autostart.shouldStart(BOOT, enabled = false, running = running, stoppedByUser = stopped))
        }
    }

    @Test
    fun updateStartsWhenEnabledOrInterrupted() {
        // Enabled: also after Stop and when it was not running (A124).
        assertTrue(Autostart.shouldStart(PACKAGE_REPLACED, enabled = true, running = false, stoppedByUser = true))
        assertTrue(Autostart.shouldStart(PACKAGE_REPLACED, enabled = true, running = false, stoppedByUser = false))
        // Disabled but running: the update only interrupted it.
        assertTrue(Autostart.shouldStart(PACKAGE_REPLACED, enabled = false, running = true, stoppedByUser = false))
        // Disabled and stopped stays stopped.
        assertFalse(Autostart.shouldStart(PACKAGE_REPLACED, enabled = false, running = false, stoppedByUser = false))
        assertFalse(Autostart.shouldStart(PACKAGE_REPLACED, enabled = false, running = false, stoppedByUser = true))
    }

    @Test
    fun appOpenStartsOnlyEnabledStoppedWorkerNotStoppedByUser() {
        assertTrue(Autostart.shouldStart(APP_OPENED, enabled = true, running = false, stoppedByUser = false))
        assertFalse(Autostart.shouldStart(APP_OPENED, enabled = true, running = false, stoppedByUser = true))
        assertFalse(Autostart.shouldStart(APP_OPENED, enabled = true, running = true, stoppedByUser = false))
        assertFalse(Autostart.shouldStart(APP_OPENED, enabled = false, running = false, stoppedByUser = false))
    }

    @Test
    fun stateLines() {
        assertEquals("Running · starts automatically", Autostart.stateLine(running = true, enabled = true))
        assertEquals("Stopped · starts automatically after restart", Autostart.stateLine(running = false, enabled = true))
        assertEquals("Running · disabled: will not start after restart", Autostart.stateLine(running = true, enabled = false))
        assertEquals("Stopped · disabled", Autostart.stateLine(running = false, enabled = false))
    }

    @Test
    fun buttonLabelsSayWhatHappens() {
        assertTrue(Autostart.stopLabel(enabled = true).contains("starts again"))
        assertEquals("Stop Worker now", Autostart.stopLabel(enabled = false))
        assertTrue(Autostart.enableLabel(running = false).contains("start now"))
        assertFalse(Autostart.enableLabel(running = true).contains("now"))
    }
}
