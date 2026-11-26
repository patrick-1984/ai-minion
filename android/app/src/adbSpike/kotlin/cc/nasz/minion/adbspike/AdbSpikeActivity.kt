// Spike K0: debug-only activity (D-pad) and pairing overlay for the device part of K0.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.adbspike

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.graphics.Color
import android.graphics.PixelFormat
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.provider.Settings
import android.text.InputType
import android.view.Gravity
import android.view.View
import android.view.WindowManager
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import java.util.concurrent.Executors

class AdbSpikeActivity : Activity() {
    private val worker = Executors.newSingleThreadExecutor()
    private lateinit var out: TextView
    private lateinit var pairPort: EditText
    private lateinit var code: EditText
    private lateinit var connectPort: EditText
    private var mode = KeyMode.KEYSTORE_RAW
    private var overlay: View? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val col = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; setPadding(48, 32, 48, 32) }
        fun field(hint: String, pw: Boolean = false) = EditText(this).apply {
            this.hint = hint
            inputType = InputType.TYPE_CLASS_NUMBER or (if (pw) InputType.TYPE_NUMBER_VARIATION_PASSWORD else 0)
            col.addView(this)
        }
        fun button(label: String, action: () -> Unit) = Button(this).apply { text = label; setOnClickListener { action() }; col.addView(this) }
        col.addView(TextView(this).apply { text = "Minion adb spike K0 (debug build only)"; textSize = 22f })
        val modeButton = button("Key: ${mode.name}") {}
        modeButton.setOnClickListener {
            mode = KeyMode.values()[(mode.ordinal + 1) % KeyMode.values().size]
            modeButton.text = "Key: ${mode.name}"
        }
        pairPort = field("pairing port (Settings > Wireless debugging > Pair with code)")
        code = field("six-digit pairing code", pw = true)
        button("Pair (in this activity)") { runPair(pairPort, code) }
        connectPort = field("connection port (Wireless debugging > IP address & port)")
        button("Connect and run: id -u") { runShell("id -u") }
        button("Connect and run: getprop ro.build.fingerprint; id") { runShell("getprop ro.build.fingerprint; id") }
        button("Connect and grant overlay to this app (appops)") { runShell("appops set $packageName SYSTEM_ALERT_WINDOW allow; appops get $packageName SYSTEM_ALERT_WINDOW") }
        button("Open pairing overlay (stays over Settings)") { showOverlay() }
        button("Key info") { bg { log("key ${mode.name}: ${AdbKeys.load(this, mode).storageDescription()}") } }
        button("Delete key of this mode") { bg { AdbKeys.delete(this, mode); log("deleted key ${mode.name}") } }
        button("Open Wireless debugging settings") {
            runCatching { startActivity(Intent(Settings.ACTION_APPLICATION_DEVELOPMENT_SETTINGS)) }.onFailure { log("open settings: $it") }
        }
        out = TextView(this).apply { textSize = 14f; setTextIsSelectable(true) }
        col.addView(out)
        setContentView(ScrollView(this).apply { addView(col) })
        log("SDK ${Build.VERSION.SDK_INT}, ABI ${Build.SUPPORTED_ABIS.joinToString()}, ${Build.FINGERPRINT}")
    }

    override fun onDestroy() {
        removeOverlay()
        worker.shutdownNow()
        super.onDestroy()
    }

    private fun runPair(portField: EditText, codeField: EditText) {
        val port = portField.text.toString().toIntOrNull() ?: return log("pairing port missing")
        val chars = CharArray(codeField.text.length).also { codeField.text.getChars(0, it.size, it, 0) }
        codeField.text.clear() // the code is never stored, logged or shown
        val m = mode
        bg { AdbBridge(AdbKeys.load(this, m), ::log).pair(port, chars); log("PAIR OK with key ${m.name}") }
    }

    private fun runShell(cmd: String) {
        val port = connectPort.text.toString().toIntOrNull() ?: return log("connection port missing")
        val m = mode
        bg {
            val t0 = System.nanoTime()
            val r = AdbBridge(AdbKeys.load(this, m), ::log).shell(port, cmd)
            log("SHELL OK (${(System.nanoTime() - t0) / 1_000_000} ms, key ${m.name}): ${r.trim()}")
        }
    }

    /** Overlay with port + code fields and a Pair button, so the Settings pairing screen is not paused. */
    private fun showOverlay() {
        if (!Settings.canDrawOverlays(this)) {
            log("overlay permission missing: opening the system screen (on TV it may not exist; see K0 runbook)")
            runCatching {
                startActivity(Intent(Settings.ACTION_MANAGE_OVERLAY_PERMISSION, Uri.parse("package:$packageName")))
            }.onFailure { log("no overlay permission screen: $it") }
            return
        }
        removeOverlay()
        val wm = getSystemService(Context.WINDOW_SERVICE) as WindowManager
        val box = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL; setBackgroundColor(Color.argb(235, 20, 20, 20)); setPadding(24, 16, 24, 16)
        }
        val p = EditText(this).apply { hint = "pairing port"; inputType = InputType.TYPE_CLASS_NUMBER; setTextColor(Color.WHITE) }
        val c = EditText(this).apply {
            hint = "code"; inputType = InputType.TYPE_CLASS_NUMBER or InputType.TYPE_NUMBER_VARIATION_PASSWORD; setTextColor(Color.WHITE)
        }
        box.addView(TextView(this).apply { text = "Minion: pair (overlay)"; setTextColor(Color.WHITE) })
        box.addView(p); box.addView(c)
        box.addView(Button(this).apply { text = "Pair"; setOnClickListener { runPair(p, c) } })
        box.addView(Button(this).apply { text = "Close"; setOnClickListener { removeOverlay() } })
        val lp = WindowManager.LayoutParams(
            WindowManager.LayoutParams.WRAP_CONTENT, WindowManager.LayoutParams.WRAP_CONTENT,
            WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY, 0, PixelFormat.TRANSLUCENT,
        ).apply { gravity = Gravity.TOP or Gravity.END }
        wm.addView(box, lp)
        overlay = box
        log("overlay shown; now open Settings > Wireless debugging > Pair device with pairing code")
    }

    private fun removeOverlay() {
        overlay?.let { (getSystemService(Context.WINDOW_SERVICE) as WindowManager).removeView(it) }
        overlay = null
    }

    private fun bg(task: () -> Unit) {
        worker.execute {
            try { task() } catch (t: Throwable) {
                var e: Throwable? = t
                val chain = StringBuilder("FAILED: ")
                while (e != null) { chain.append(e.javaClass.simpleName).append(": ").append(e.message).append(" <- "); e = e.cause }
                log(chain.toString())
            }
        }
    }

    private fun log(line: String) {
        runOnUiThread { out.append(line + "\n") }
    }
}
