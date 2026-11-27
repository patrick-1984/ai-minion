// Device control: the pairing keypad (design 4.2). A TYPE_APPLICATION_OVERLAY window over TV Settings, so the
// Settings pairing screen stays resumed. Exists only while pairing (3 minutes at most). The code lives in a
// CharArray in this process only, is shown as dots, and is wiped after use; no EditText, no IME, no autofill.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import android.content.Context
import android.graphics.Color
import android.graphics.PixelFormat
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.StateListDrawable
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.view.Gravity
import android.view.KeyEvent
import android.view.View
import android.view.WindowManager
import android.widget.Button
import android.widget.GridLayout
import android.widget.LinearLayout
import android.widget.TextView

class PairingOverlay(context: Context, private val control: DeviceControl, private val finished: (String?) -> Unit) {
    private val ctx = context.applicationContext
    private val wm = ctx.getSystemService(WindowManager::class.java)
    private val main = Handler(Looper.getMainLooper())
    private val code = CharArray(CODE_LENGTH)
    private var count = 0
    private val port = CharArray(5)
    private var portCount = 0
    private var editingPort = false
    private var root: LinearLayout? = null
    private lateinit var codeView: TextView
    private lateinit var portView: TextView
    private lateinit var message: TextView
    private var busy = false
    private val timeout = Runnable { close("Pairing timed out. Start again from Device control.") }
    // Digit keys of a remote are accepted too; Back cancels.
    private val keys = View.OnKeyListener { _, keyCode, event ->
        if (event.action != KeyEvent.ACTION_DOWN) return@OnKeyListener false
        when (keyCode) {
            in KeyEvent.KEYCODE_0..KeyEvent.KEYCODE_9 -> { digit('0' + (keyCode - KeyEvent.KEYCODE_0)); true }
            KeyEvent.KEYCODE_DEL -> { back(); true }
            KeyEvent.KEYCODE_BACK, KeyEvent.KEYCODE_ESCAPE -> { close("Pairing cancelled."); true }
            else -> false
        }
    }
    private val refresh = object : Runnable {
        override fun run() {
            if (portCount == 0 && !editingPort) control.pairingPortHint()?.let { setPort(it) }
            if (root != null) main.postDelayed(this, 1_000)
        }
    }

    fun show(): Boolean {
        if (!Settings.canDrawOverlays(ctx) || root != null) return false
        control.startPairing()
        val v = layout()
        val params = WindowManager.LayoutParams(
            WindowManager.LayoutParams.WRAP_CONTENT, WindowManager.LayoutParams.WRAP_CONTENT,
            WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY,
            WindowManager.LayoutParams.FLAG_SECURE or WindowManager.LayoutParams.FLAG_LAYOUT_IN_SCREEN,
            PixelFormat.TRANSLUCENT,
        ).apply { gravity = Gravity.TOP or Gravity.END; x = dp(24); y = dp(24) }
        wm.addView(v, params)
        root = v
        main.postDelayed(timeout, TIMEOUT_MS)
        main.post(refresh)
        return true
    }

    fun close(result: String?) {
        main.removeCallbacks(timeout)
        main.removeCallbacks(refresh)
        wipe()
        root?.let { runCatching { wm.removeView(it) } }
        root = null
        control.stopPairing()
        finished(result)
    }

    private fun wipe() {
        code.fill('\u0000'); count = 0
    }

    private fun setPort(p: Int) {
        val s = p.toString()
        port.fill('\u0000'); portCount = 0
        for (ch in s) if (portCount < port.size) port[portCount++] = ch
        render()
    }

    private fun digit(d: Char) {
        if (busy) return
        if (editingPort) { if (portCount < port.size) port[portCount++] = d }
        else if (count < CODE_LENGTH) code[count++] = d
        render()
    }

    private fun back() {
        if (busy) return
        if (editingPort) { if (portCount > 0) port[--portCount] = '\u0000' }
        else if (count > 0) code[--count] = '\u0000'
        render()
    }

    private fun submit() {
        if (busy) return
        if (editingPort) { editingPort = false; render(); return }
        val p = String(port, 0, portCount).toIntOrNull()
        if (p == null || !EndpointChooser.validPort(p)) { message.text = "Enter the pairing port shown in Settings (Edit port)."; return }
        if (count != CODE_LENGTH) { message.text = "Enter all $CODE_LENGTH digits of the pairing code."; return }
        busy = true
        val attempt = code.copyOf(count)
        wipe()
        render()
        message.text = "Pairing..."
        control.pair(p, attempt) { error ->
            main.post {
                busy = false
                if (error == null) {
                    message.text = "Paired as ${AdbKeys.NAME}."
                    main.postDelayed({ close(null) }, 2_500)
                } else {
                    // Design 4.2: no retry with the same code.
                    close(error)
                }
            }
        }
    }

    private fun render() {
        codeView.text = (0 until CODE_LENGTH).joinToString(" ") { if (it < count) "●" else "_" }
        portView.text = "Pairing port: " + (if (portCount == 0) "?" else String(port, 0, portCount)) + if (editingPort) "  (typing)" else ""
    }

    private fun layout(): LinearLayout {
        val box = LinearLayout(ctx).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(18), dp(14), dp(18), dp(14))
            background = surface("#E61B2226", "#83E2BA", 2)
            isFocusable = true
            setOnKeyListener(keys)
        }
        box.addView(text("Minion: pair Device control", 18f, true))
        box.addView(text("In Settings open Wireless debugging > Pair device with pairing code, then type the code here.", 14f, false)
            .apply { maxWidth = dp(360) })
        portView = text("", 15f, false)
        box.addView(portView)
        codeView = text("", 26f, true).apply { typeface = android.graphics.Typeface.MONOSPACE }
        box.addView(codeView)
        message = text("", 14f, false).apply { maxWidth = dp(360) }
        box.addView(message)
        val grid = GridLayout(ctx).apply { columnCount = 3 }
        val keys = listOf("1", "2", "3", "4", "5", "6", "7", "8", "9", "⌫", "0", "OK")
        var first: Button? = null
        for (k in keys) {
            val b = key(k) {
                when (k) {
                    "⌫" -> back()
                    "OK" -> submit()
                    else -> digit(k[0])
                }
            }
            if (first == null) first = b
            grid.addView(b)
        }
        box.addView(grid)
        val row = LinearLayout(ctx).apply { orientation = LinearLayout.HORIZONTAL }
        row.addView(key("Edit port") { editingPort = true; port.fill('\u0000'); portCount = 0; render() })
        row.addView(key("Cancel") { close("Pairing cancelled.") })
        box.addView(row)
        render()
        first?.post { first.requestFocus() }
        return box
    }

    private fun text(s: String, size: Float, bold: Boolean) = TextView(ctx).apply {
        text = s; textSize = size; setTextColor(Color.WHITE)
        if (bold) setTypeface(typeface, android.graphics.Typeface.BOLD)
        setPadding(0, dp(4), 0, dp(4))
    }

    private fun key(label: String, action: () -> Unit) = Button(ctx).apply {
        text = label; isAllCaps = false; textSize = 18f; setTextColor(Color.WHITE)
        minWidth = dp(64); minHeight = dp(48)
        isFocusable = true
        setOnKeyListener(keys)
        background = StateListDrawable().apply {
            addState(intArrayOf(android.R.attr.state_focused), surface("#295446", "#83E2BA", 2))
            addState(intArrayOf(), surface("#252F35", "#3B4B51", 1))
        }
        setOnClickListener { action() }
    }

    private fun surface(fill: String, stroke: String, line: Int) = GradientDrawable().apply {
        setColor(Color.parseColor(fill)); setStroke(dp(line), Color.parseColor(stroke)); cornerRadius = dp(9).toFloat()
    }

    private fun dp(v: Int) = (v * ctx.resources.displayMetrics.density + 0.5f).toInt()

    companion object {
        const val CODE_LENGTH = 6
        const val TIMEOUT_MS = 180_000L
    }
}
