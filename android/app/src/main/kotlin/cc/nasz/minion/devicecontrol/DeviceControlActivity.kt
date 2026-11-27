// Device control screen (D-pad): enable with the plain statement, the transport in use (legacy adb on 127.0.0.1:5555
// or Wireless debugging TLS) and the first-connection hint for the TV dialog, pair through the overlay keypad, manual
// connection port, status, disable, Forget guidance. Declared only in -PminionDeviceControl=true builds and not
// exported; MainActivity shows its button only when DeviceControl.available.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import android.app.Activity
import android.app.AlertDialog
import android.content.Intent
import android.graphics.Color
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.StateListDrawable
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.text.InputFilter
import android.text.InputType
import android.view.Gravity
import android.view.View
import android.view.WindowManager
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView

class DeviceControlActivity : Activity() {
    private lateinit var control: DeviceControl
    private val main = Handler(Looper.getMainLooper())
    private lateinit var status: TextView
    private lateinit var detail: TextView
    private lateinit var notice: TextView
    private lateinit var enable: Button
    private lateinit var pair: Button
    private lateinit var port: Button
    private lateinit var disable: Button
    private lateinit var forget: Button
    private var overlay: PairingOverlay? = null
    private val listener: () -> Unit = { main.post { render() } }
    private val tick = object : Runnable { override fun run() { render(); main.postDelayed(this, 2_000) } }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        if (!DeviceControl.available) { finish(); return }
        control = DeviceControl.get(this)
        setContentView(screen())
    }

    override fun onStart() {
        super.onStart()
        if (!DeviceControl.available) return
        control.observe(listener)
        main.post(tick)
    }

    override fun onStop() {
        if (DeviceControl.available) control.unobserve(listener)
        main.removeCallbacks(tick)
        super.onStop()
    }

    private fun screen(): View {
        val page = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; setPadding(dp(48), dp(28), dp(48), dp(28)) }
        page.addView(label("Device control", 32f, Color.WHITE, true))
        status = label("", 22f, mint, true); page.addView(status)
        detail = label("", 16f, ink); page.addView(detail)
        notice = label("", 16f, Color.parseColor("#F3C969")); page.addView(notice)
        enable = button("Enable device control") { confirmEnable() }
        pair = button("Pair Wireless debugging (type the code from Settings)") { startPairing() }
        port = button("Wireless debugging connection port") { askPort() }
        disable = button("Disable device control") { control.disable("app"); notice.text = control.forgetGuidance() }
        forget = button("How to remove the authorization from Android") {
            AlertDialog.Builder(this).setTitle("Forget in Android").setMessage(control.forgetGuidance())
                .setPositiveButton("OK", null).show()
        }
        for (b in listOf(enable, pair, port, disable, forget)) page.addView(b, row(10))
        page.addView(label(
            "Uses adb on 127.0.0.1:5555 when this TV has it (no Wi-Fi needed; the first connection asks \"Allow debugging?\" " +
                "on the TV once, tick \"Always allow\"). Otherwise Wireless debugging, which needs Wi-Fi and a pairing. " +
                "Only for a dedicated Worker that is not shared.",
            15f, ink), row(16))
        return ScrollView(this).apply { addView(page) }
    }

    private fun render() {
        if (isFinishing) return
        status.text = control.statusLine()
        val s = control.status()
        detail.text = buildString {
            append("State ").append(s.optString("state"))
            when (s.optString("transport")) {
                DeviceControl.TRANSPORT_LEGACY -> append("\nTransport legacy adb (127.0.0.1:5555)")
                DeviceControl.TRANSPORT_TLS -> append("\nTransport Wireless debugging (TLS, 127.0.0.1:").append(s.optInt("port")).append(")")
            }
            control.manualPort?.let { append("\nConnection port (manual) ").append(it) }
            s.optString("key_storage").takeIf { it.isNotEmpty() }?.let { append("\nKey ").append(it) }
            s.optString("key_warning").takeIf { it.isNotEmpty() }?.let { append("\n").append(it) }
            s.optString("last_error_message").takeIf { it.isNotEmpty() }?.let { append("\nLast error: ").append(it) }
        }
        if (s.optString("state") == DcState.AWAITING_AUTHORIZATION.name) {
            notice.text = "First connection: look at the TV. ${LegacyConnector.CONFIRM}"
        }
        val on = control.enabled
        enable.visibility = if (on) View.GONE else View.VISIBLE
        pair.visibility = if (on) View.VISIBLE else View.GONE
        port.visibility = if (on) View.VISIBLE else View.GONE
        disable.visibility = if (on) View.VISIBLE else View.GONE
        if (currentFocus == null) listOf(enable, pair).firstOrNull { it.visibility == View.VISIBLE }?.requestFocus()
    }

    private fun confirmEnable() {
        AlertDialog.Builder(this)
            .setTitle("Enable device control?")
            .setMessage(
                "Sessions of this Worker get shell-level control of this TV (install apps, change settings, read the " +
                    "screen, send key presses) while adb is on (adb on 127.0.0.1:5555 or Wireless debugging).\n\nOnly for a dedicated Worker that is not shared. " +
                    "You can disable it here or on the Hub at any time."
            )
            .setNegativeButton("Cancel", null)
            .setPositiveButton("Enable") { _, _ -> notice.text = control.enable() ?: ""; render() }
            .show()
    }

    private fun startPairing() {
        if (!Settings.canDrawOverlays(this)) {
            notice.text = "Allow \"Display over other apps\" for Minion, then press Pair again. It is used only while pairing."
            runCatching {
                startActivity(Intent(Settings.ACTION_MANAGE_OVERLAY_PERMISSION, Uri.parse("package:$packageName")))
            }.onFailure {
                notice.text = "This device hides \"Display over other apps\". See the Minion guide for the one-time PC step."
            }
            return
        }
        val o = PairingOverlay(this, control) { result ->
            overlay = null
            main.post {
                notice.text = result ?: ("Paired. You may revoke \"Display over other apps\" for Minion again.")
                render()
            }
        }
        if (!o.show()) return
        overlay = o
        notice.text = "Now open Settings > Developer options > Wireless debugging > Pair device with pairing code."
        runCatching { startActivity(Intent(Settings.ACTION_APPLICATION_DEVELOPMENT_SETTINGS)) }
    }

    private fun askPort() {
        val input = EditText(this).apply {
            hint = "Port from Wireless debugging > IP address & Port (empty = automatic)"
            inputType = InputType.TYPE_CLASS_NUMBER
            filters = arrayOf(InputFilter.LengthFilter(5))
            isSaveEnabled = false
            importantForAutofill = View.IMPORTANT_FOR_AUTOFILL_NO
            setText(control.manualPort?.toString().orEmpty())
        }
        AlertDialog.Builder(this)
            .setTitle("Connection port")
            .setView(input)
            .setNegativeButton("Cancel", null)
            .setPositiveButton("Save") { _, _ ->
                val p = input.text.toString().trim().toIntOrNull()
                control.setManualPort(p?.takeIf { EndpointChooser.validPort(it) })
                render()
            }
            .show()
    }

    private fun label(text: String, size: Float, color: Int, bold: Boolean = false) = TextView(this).apply {
        this.text = text; textSize = size; setTextColor(color)
        if (bold) setTypeface(typeface, Typeface.BOLD)
        setPadding(0, dp(4), 0, dp(4))
    }

    private fun button(text: String, action: () -> Unit) = Button(this).apply {
        this.text = text; isAllCaps = false; minHeight = dp(48); textSize = 18f
        gravity = Gravity.CENTER_VERTICAL or Gravity.START
        setTextColor(Color.WHITE)
        background = StateListDrawable().apply {
            addState(intArrayOf(android.R.attr.state_focused), surface("#295446", "#83E2BA", 2))
            addState(intArrayOf(), surface("#252F35", "#3B4B51", 1))
        }
        setOnClickListener { action() }
    }

    private fun surface(fill: String, stroke: String, line: Int) = GradientDrawable().apply {
        setColor(Color.parseColor(fill)); setStroke(dp(line), Color.parseColor(stroke)); cornerRadius = dp(9).toFloat()
    }

    private fun dp(v: Int) = (v * resources.displayMetrics.density + 0.5f).toInt()
    private fun row(top: Int) = LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT)
        .apply { topMargin = dp(top) }

    private val mint = Color.parseColor("#83E2BA")
    private val ink = Color.parseColor("#CFD8DA")
}
