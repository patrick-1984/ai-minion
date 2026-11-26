package cc.nasz.minion

import android.Manifest
import android.app.Activity
import android.app.AlertDialog
import android.app.UiModeManager
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.ServiceConnection
import android.content.pm.PackageManager
import android.content.res.Configuration
import android.graphics.Color
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.StateListDrawable
import android.os.Build
import android.os.Bundle
import android.os.IBinder
import android.text.InputFilter
import android.text.InputType
import android.view.Gravity
import android.view.View
import android.view.WindowInsets
import android.view.WindowManager
import android.view.inputmethod.EditorInfo
import android.widget.Button
import android.widget.CheckBox
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import java.net.URI
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter

class MainActivity : Activity() {
    private var worker: WorkerService.LocalBinder? = null
    private var bound = false
    private var model = WorkerView()
    private var drawing = false
    private var television = false

    private lateinit var status: TextView
    private lateinit var notice: TextView
    private lateinit var hub: TextView
    private lateinit var dialOverride: TextView
    private lateinit var workerWords: TextView
    private lateinit var hubWords: TextView
    private lateinit var pairing: TextView
    private lateinit var expires: TextView
    private lateinit var sessions: TextView
    private lateinit var usageStatus: TextView
    private lateinit var share: CheckBox
    private lateinit var boot: CheckBox
    private lateinit var pair: Button
    private lateinit var token: Button
    private lateinit var start: Button
    private lateinit var pause: Button
    private lateinit var stop: Button
    private lateinit var forget: Button
    private lateinit var clearOverride: Button
    private lateinit var reviewUsage: Button

    private val service = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName, binder: IBinder) {
            worker = binder as WorkerService.LocalBinder
            worker?.observe(::render)
        }

        override fun onServiceDisconnected(name: ComponentName) {
            worker = null
            render(WorkerView(status = "Service unavailable"))
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // The launcher is exported for TV and phone launchers. Its intent is
        // never a source of Worker settings, commands, or credentials.
        television = getSystemService(UiModeManager::class.java).currentModeType == Configuration.UI_MODE_TYPE_TELEVISION
        setContentView(screen())
        render(model)
    }

    override fun onStart() {
        super.onStart()
        bound = bindService(Intent(this, WorkerService::class.java), service, Context.BIND_AUTO_CREATE)
    }

    override fun onStop() {
        worker?.observe(null)
        if (bound) unbindService(service)
        bound = false
        worker = null
        super.onStop()
    }

    private fun screen(): View {
        val scroll = ScrollView(this).apply { isFillViewport = true }
        val inset = if (television) 48 else 22
        scroll.setPadding(dp(inset), dp(28), dp(inset), dp(28))
        scroll.clipToPadding = false
        if (Build.VERSION.SDK_INT >= 30) {
            scroll.setOnApplyWindowInsetsListener { view, insets ->
                val bars = insets.getInsets(WindowInsets.Type.systemBars() or WindowInsets.Type.displayCutout())
                view.setPadding(dp(inset) + bars.left, dp(28) + bars.top, dp(inset) + bars.right, dp(28) + bars.bottom)
                insets
            }
        } else {
            scroll.fitsSystemWindows = true
        }
        val page = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        val available = resources.configuration.screenWidthDp - inset * 2
        val width = if (available > 760) dp(760) else FrameLayout.LayoutParams.MATCH_PARENT
        scroll.addView(page, FrameLayout.LayoutParams(width, FrameLayout.LayoutParams.WRAP_CONTENT, Gravity.TOP or Gravity.CENTER_HORIZONTAL))
        page.addView(label(getString(R.string.app_name), if (television) 36f else 30f, Color.WHITE, true))
        status = label("", if (television) 25f else 21f, mint, true)
        page.addView(status, row(8))
        notice = label("", 16f, ink)
        notice.accessibilityLiveRegion = View.ACCESSIBILITY_LIVE_REGION_POLITE
        page.addView(notice, row(5))

        val details = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            background = surface("#20272D", "#3A484D")
            setPadding(dp(16), dp(12), dp(16), dp(14))
        }
        page.addView(details, row(20))
        hub = label("", 16f, ink)
        details.addView(hub)
        dialOverride = label("", 16f, ink)
        details.addView(dialOverride, row(8))
        pairing = label("", if (television) 32f else 26f, mint, true).apply { typeface = Typeface.MONOSPACE }
        details.addView(pairing, row(8))
        expires = label("", 15f, ink)
        details.addView(expires, row(4))
        hubWords = label("", 18f, Color.WHITE)
        details.addView(hubWords, row(10))
        workerWords = label("", 18f, Color.WHITE)
        details.addView(workerWords, row(10))
        sessions = label("", 16f, ink)
        details.addView(sessions, row(8))
        usageStatus = label("", 16f, ink)
        details.addView(usageStatus, row(8))

        pair = button("Pair with a Hub") { showJoin(byToken = false) }
        token = button("Use a join token") { showJoin(byToken = true) }
        start = button("Start Worker") { worker?.start() }
        pause = button("Pause") { worker?.pause(model.status == "Paused") }
        stop = button("Stop Worker") {
            if (model.sessions.toIntOrNull()?.let { it > 0 } == true) {
                AlertDialog.Builder(this)
                    .setTitle("Stop Worker?")
                    .setMessage("The Worker disconnects and closes its terminal sessions.")
                    .setNegativeButton("Cancel", null)
                    .setPositiveButton("Stop") { _, _ -> worker?.stop() }.show()
            } else {
                worker?.stop()
            }
        }
        for (action in listOf(pair, token, start, pause, stop)) page.addView(action, row(10))

        share = CheckBox(this).apply {
            text = "Share terminal sessions with the Hub"
            textSize = if (television) 18f else 16f
            minHeight = dp(48)
            setOnCheckedChangeListener { _, value -> if (!drawing) worker?.share(value) }
        }
        boot = CheckBox(this).apply {
            text = "Restart after reboot and first unlock"
            textSize = if (television) 18f else 16f
            minHeight = dp(48)
            setOnCheckedChangeListener { _, value -> if (!drawing) worker?.boot(value) }
        }
        page.addView(share, row(12))
        page.addView(boot, row(2))
        reviewUsage = button("Review usage monitoring") { model.usage?.let(::showUsage) }
        page.addView(reviewUsage, row(6))
        forget = button("Forget this Hub") {
            AlertDialog.Builder(this)
                .setTitle("Forget this Hub?")
                .setMessage("This deletes the Worker identity and the saved Hub connection. Remove the old Worker in the Hub as well.")
                .setNegativeButton("Cancel", null)
                .setPositiveButton("Forget") { _, _ -> worker?.forget() }.show()
        }
        page.addView(forget, row(6))
        clearOverride = button("Clear Hub address override") {
            AlertDialog.Builder(this)
                .setTitle("Clear the Hub address override?")
                .setMessage("The Worker looks up the Hub's name with this network's DNS again from its next start.")
                .setNegativeButton("Cancel", null)
                .setPositiveButton("Clear") { _, _ -> worker?.clearDialOverride() }.show()
        }
        page.addView(clearOverride, row(6))
        return scroll
    }

    private fun render(value: WorkerView) {
        model = value
        drawing = true
        status.text = value.status
        notice.show(value.message)
        hub.show(if (value.hub.isBlank()) "No Hub configured" else "Hub\n${value.hub}")
        dialOverride.show(if (value.dialOverride.isBlank()) "" else
            "Hub address override\n${HubDialOverride.describe(value.dialOverride)} (TLS still checks the name)")
        if (value.code.isBlank()) {
            pairing.show("")
            expires.show("")
            window.clearFlags(WindowManager.LayoutParams.FLAG_SECURE)
        } else {
            window.addFlags(WindowManager.LayoutParams.FLAG_SECURE)
            pairing.show(value.code)
            val deadline = runCatching {
                DateTimeFormatter.ofPattern("h:mm a").withZone(ZoneId.systemDefault()).format(Instant.parse(value.expires))
            }.getOrNull()
            expires.show("Compare the Hub words, then enter this code in Workers > Add worker." +
                if (deadline == null) "" else " Expires at $deadline.")
        }
        hubWords.show(if (value.hubWords.isBlank()) "" else
            "${if (value.paired) "Hub key words" else "Presented Hub key words"}\n${value.hubWords}")
        workerWords.show(if (value.workerWords.isBlank()) "" else "Worker key words\n${value.workerWords}")
        sessions.show(if (value.sessions.isBlank()) "" else "Terminal sessions  ${value.sessions}")

        usageStatus.show(when (value.usage?.accepted) {
            true -> "Usage monitoring accepted"
            false -> if (value.usage.required) "Usage monitoring pending local acceptance" else "Usage monitoring acceptance is available"
            null -> ""
        })
        val configured = value.hub.isNotBlank()
        pair.visible(!configured && !value.busy)
        token.visible(!configured && !value.busy)
        start.visible(configured && !value.wanted && !value.busy)
        pause.visible(value.wanted && !value.busy)
        pause.text = if (value.status == "Paused") "Resume" else "Pause"
        stop.visible(value.wanted || value.busy)
        share.visible(configured && !value.busy)
        boot.visible(configured && !value.busy)
        reviewUsage.visible(value.usage?.accepted == false && !value.busy)
        forget.visible(configured && !value.wanted && !value.busy)
        clearOverride.visible(value.dialOverride.isNotBlank() && !value.wanted && !value.busy)
        val controls = listOf(pair, token, start, pause, stop, share, boot, reviewUsage, forget, clearOverride)
        for (control in controls) control.isEnabled = worker != null
        if (television && currentFocus == null) {
            controls.firstOrNull { it.isEnabled && it.visibility == View.VISIBLE }?.requestFocus()
        }
        share.isChecked = value.share
        boot.isChecked = value.boot
        drawing = false
    }

    private fun showJoin(byToken: Boolean) {
        val input = EditText(this).apply {
            hint = if (byToken) "Paste the join token" else "https://hub.example"
            setSingleLine()
            filters = arrayOf(InputFilter.LengthFilter(if (byToken) 4096 else 1024))
            isSaveEnabled = false
            importantForAutofill = View.IMPORTANT_FOR_AUTOFILL_NO
            inputType = InputType.TYPE_CLASS_TEXT or if (byToken) {
                InputType.TYPE_TEXT_VARIATION_PASSWORD
            } else InputType.TYPE_TEXT_VARIATION_URI
            imeOptions = EditorInfo.IME_ACTION_DONE or EditorInfo.IME_FLAG_NO_PERSONALIZED_LEARNING
        }
        val selection = CheckBox(this).apply {
            text = "Share this device's terminal sessions with the Hub"
            isChecked = true
        }
        val disclosure = TextView(this).apply {
            setText(R.string.usage_enrollment_disclosure)
            textSize = 16f
        }
        val usage = CheckBox(this).apply {
            setText(R.string.accept_usage_enrollment)
            isSaveEnabled = false
            setOnCheckedChangeListener { _, checked -> if (checked) error = null }
        }
        val overrideHelp = TextView(this).apply {
            text = "Advanced: Hub address override (optional)\n" +
                "Use only when this network's DNS cannot find the Hub. Enter the Hub's IP address, such as 10.3.3.6; " +
                "the name comes from the Hub URL or the join token. " +
                "TLS still checks the Hub's certificate name."
            textSize = 16f
        }
        // A plain text field: the D-pad reaches it in form order and the
        // on-screen keyboard of a TV remote can type it.
        val overrideInput = EditText(this).apply {
            id = View.generateViewId()
            hint = "IP address (leave empty for none)"
            setSingleLine()
            filters = arrayOf(InputFilter.LengthFilter(64))
            isSaveEnabled = false
            importantForAutofill = View.IMPORTANT_FOR_AUTOFILL_NO
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_URI
            imeOptions = EditorInfo.IME_ACTION_DONE or EditorInfo.IME_FLAG_NO_PERSONALIZED_LEARNING
        }
        val form = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(22), dp(12), dp(22), 0)
            addView(input)
            addView(disclosure, row(12))
            addView(usage, row(8))
            addView(selection, row(12))
            addView(overrideHelp, row(16))
            addView(overrideInput, row(4))
        }
        val dialog = AlertDialog.Builder(this)
            .setTitle(if (byToken) "Join a Hub" else "Pair with a Hub")
            .setView(ScrollView(this).apply { addView(form) })
            .setNegativeButton("Cancel", null)
            .setPositiveButton(if (byToken) "Join" else "Show pairing code", null)
            .create()
        if (byToken) dialog.window?.addFlags(WindowManager.LayoutParams.FLAG_SECURE)
        dialog.setOnShowListener {
            val submit = dialog.getButton(AlertDialog.BUTTON_POSITIVE)
            submit.setOnClickListener {
                val value = input.text.toString().trim()
                val valid = if (byToken) value.startsWith("mnj1-") && value.none(Char::isWhitespace)
                    else validServer(value)
                if (!valid) {
                    input.error = if (byToken) "Paste a Minion join token from the Hub." else "Enter a full Hub URL, such as https://hub.example"
                    return@setOnClickListener
                }
                val overrideText = overrideInput.text.toString().trim()
                var dialValue = ""
                if (overrideText.isNotEmpty()) {
                    val host = if (byToken) HubDialOverride.hostOfToken(value) else HubDialOverride.hostOfServer(value)
                    val address = HubDialOverride.address(overrideText)
                    if (host == null) {
                        overrideInput.error = "The Hub address has no hostname to override. Leave this field empty."
                        overrideInput.requestFocus()
                        return@setOnClickListener
                    }
                    if (address == null) {
                        overrideInput.error = "Enter an IPv4 or IPv6 address, such as 10.3.3.6, without a port."
                        overrideInput.requestFocus()
                        return@setOnClickListener
                    }
                    dialValue = "$host=$address"
                }
                if (!usage.isChecked) {
                    usage.error = getString(R.string.usage_enrollment_required)
                    usage.requestFocus()
                    return@setOnClickListener
                }
                val target = worker ?: return@setOnClickListener
                val selectionValue = if (selection.isChecked) "all" else "none"
                if (byToken) target.join(value, selectionValue, usage.isChecked, dialValue)
                else target.pair(value, selectionValue, usage.isChecked, dialValue)
                input.text.clear()
                dialog.dismiss()
                if (!television && Build.VERSION.SDK_INT >= 33 &&
                    checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED &&
                    !workerPreferences(this).getBoolean("notification_asked", false)) {
                    workerPreferences(this).edit().putBoolean("notification_asked", true).apply()
                    requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
                }
            }
            val done = TextView.OnEditorActionListener { _, action, _ ->
                if (action != EditorInfo.IME_ACTION_DONE) false else {
                    submit.performClick()
                    true
                }
            }
            input.setOnEditorActionListener(done)
            overrideInput.setOnEditorActionListener(done)
        }
        dialog.setOnDismissListener { input.text.clear() }
        dialog.show()
    }

    private fun showUsage(registration: UsageRegistration) {
        val message = getString(R.string.usage_enrollment_disclosure) +
            "\n\nHub fingerprint\n${registration.hubFingerprint}" +
            "\n\nWorker registration\n${registration.workerId}" +
            "\n\nOwner\n${registration.owner}"
        AlertDialog.Builder(this)
            .setTitle("Usage monitoring")
            .setMessage(message)
            .setNegativeButton("Cancel", null)
            .setPositiveButton("Accept") { _, _ -> worker?.acceptUsage(registration) }
            .show()
    }

    private fun validServer(value: String): Boolean = runCatching {
        val uri = URI(value)
        uri.scheme in setOf("https", "http", "wss", "ws") && uri.host != null &&
            uri.userInfo == null && uri.rawQuery == null && uri.rawFragment == null
    }.getOrDefault(false)

    private fun TextView.show(value: String) {
        text = value
        visible(value.isNotEmpty())
    }

    private fun View.visible(show: Boolean) {
        visibility = if (show) View.VISIBLE else View.GONE
    }

    private fun label(text: String, size: Float, color: Int, bold: Boolean = false) = TextView(this).apply {
        this.text = text
        textSize = size
        setTextColor(color)
        setLineSpacing(dp(2).toFloat(), 1f)
        if (bold) setTypeface(typeface, Typeface.BOLD)
    }

    private fun button(text: String, action: () -> Unit) = Button(this).apply {
        id = View.generateViewId()
        this.text = text
        isAllCaps = false
        minHeight = dp(48)
        textSize = if (television) 18f else 16f
        gravity = Gravity.CENTER_VERTICAL or Gravity.START
        setPadding(dp(16), dp(8), dp(16), dp(8))
        setTextColor(Color.WHITE)
        background = StateListDrawable().apply {
            addState(intArrayOf(android.R.attr.state_focused), surface("#295446", "#83E2BA", 2))
            addState(intArrayOf(android.R.attr.state_pressed), surface("#295446", "#83E2BA", 2))
            addState(intArrayOf(), surface("#252F35", "#3B4B51"))
        }
        setOnClickListener { action() }
    }

    private fun surface(fill: String, stroke: String, line: Int = 1) = GradientDrawable().apply {
        setColor(Color.parseColor(fill))
        setStroke(dp(line), Color.parseColor(stroke))
        cornerRadius = dp(9).toFloat()
    }

    private fun dp(value: Int) = (value * resources.displayMetrics.density + 0.5f).toInt()
    private fun row(top: Int) = LinearLayout.LayoutParams(
        LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT
    ).apply { topMargin = dp(top) }

    private val mint = Color.parseColor("#83E2BA")
    private val ink = Color.parseColor("#CFD8DA")
}
