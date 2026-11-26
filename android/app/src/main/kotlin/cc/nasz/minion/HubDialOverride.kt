package cc.nasz.minion

import android.content.Context
import android.net.InetAddresses
import android.util.Base64
import org.json.JSONObject
import java.net.URI
import java.nio.charset.StandardCharsets.UTF_8

// The Hub address override dials the Hub's hostname at a fixed IP address on
// a network whose DNS cannot resolve it. Only the dial target changes: the
// Worker still sends the hostname as TLS SNI and HTTP Host and checks the
// certificate against it. The value lives in the app's preferences and is
// passed to every native process as MINION_HUB_DIAL_ADDRESS=hostname=address.
internal object HubDialOverride {
    const val PREFERENCE = "hub_dial_address"
    const val ENVIRONMENT = "MINION_HUB_DIAL_ADDRESS"

    fun saved(context: Context): String =
        workerPreferences(context).getString(PREFERENCE, null).orEmpty()

    // The hostname of a Hub URL, or null when it has none or is an address.
    fun hostOfServer(server: String): String? = runCatching {
        URI(server.trim()).host?.trimEnd('.')?.lowercase()?.takeIf(::validHostname)
    }.getOrNull()

    // The hostname of the Hub URL inside a join token (mnj1-<base64url JSON>).
    fun hostOfToken(token: String): String? = runCatching {
        val payload = Base64.decode(token.removePrefix("mnj1-"), Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP)
        hostOfServer(JSONObject(String(payload, UTF_8)).getString("hub"))
    }.getOrNull()

    // The canonical form of an IPv4 or IPv6 literal, or null. No zone, no
    // port, no name: this never reaches DNS.
    fun address(value: String): String? {
        val text = value.trim().removePrefix("[").removeSuffix("]")
        if (text.isEmpty() || '%' in text || !InetAddresses.isNumericAddress(text)) return null
        return runCatching { InetAddresses.parseNumericAddress(text).hostAddress }.getOrNull()
    }

    fun validHostname(host: String): Boolean {
        if (host.isEmpty() || host.length > 253 || ':' in host || host.startsWith("[")) return false
        val labels = host.split('.')
        val label = Regex("[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?")
        return labels.all(label::matches) && labels.last().any { it !in '0'..'9' }
    }

    // "hostname at address" for the status screen.
    fun describe(value: String): String = value.substringBefore('=') + " at " + value.substringAfter('=')
}
