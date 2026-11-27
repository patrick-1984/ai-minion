package cc.nasz.minion

/**
 * Whether the Worker starts by itself (A122, A124), kept apart from whether it
 * runs now. Pure Kotlin, so the rules are unit tested without Android.
 *
 * - Enabled (the default): the Worker starts after a device restart, after an
 *   app install or update, and when the app opens, unless Stop was pressed
 *   since the last restart or update.
 * - Disabled: nothing starts it by itself. A running Worker keeps running
 *   (also across an app update, which only interrupts it), but after the next
 *   device restart it stays stopped until Enable or Start Worker.
 * - Stop stops it now and leaves Enabled as it was.
 */
internal object Autostart {
    /** Saved choice; the historic key of "Start Minion automatically when this device turns on". */
    const val ENABLED = "boot"
    /** The Worker should run (saved by the service on start, cleared on stop). */
    const val WANTED = "wanted"
    /** Stop was pressed since the last device restart or app update. */
    const val STOPPED_BY_USER = "stopped_by_user"

    enum class Trigger { BOOT, PACKAGE_REPLACED, APP_OPENED }

    /**
     * A missing choice means the person never chose: Enabled. This is the
     * default of fresh installs and the migration of earlier installs, whose
     * explicit Yes or No (the A112 question or the checkbox) is kept.
     */
    fun enabled(saved: Boolean?): Boolean = saved ?: true

    /**
     * Whether [trigger] starts the Worker. [running] is the saved intent to run
     * for a restart or an update, and the service's live state when the app opens.
     */
    fun shouldStart(trigger: Trigger, enabled: Boolean, running: Boolean, stoppedByUser: Boolean): Boolean =
        when (trigger) {
            // A new boot: nothing runs yet, and an earlier Stop is over.
            Trigger.BOOT -> enabled
            // The update interrupted a running Worker: it comes back even when Disabled.
            Trigger.PACKAGE_REPLACED -> enabled || running
            Trigger.APP_OPENED -> enabled && !running && !stoppedByUser
        }

    /** The one-line state below the status, for example "Running · starts automatically". */
    fun stateLine(running: Boolean, enabled: Boolean): String = when {
        running && enabled -> "Running · starts automatically"
        running -> "Running · disabled: will not start after restart"
        enabled -> "Stopped · starts automatically after restart"
        else -> "Stopped · disabled"
    }

    /** Text of the Stop button: says that Enabled stays. */
    fun stopLabel(enabled: Boolean): String =
        if (enabled) "Stop Worker now (starts again after a restart or update)" else "Stop Worker now"

    /** Text of the Enable button: Enable also starts a stopped Worker. */
    fun enableLabel(running: Boolean): String =
        if (running) "Enable: start after every restart" else "Enable: start now and after every restart"

    const val DISABLE_LABEL = "Disable: do not start after a restart"
}
