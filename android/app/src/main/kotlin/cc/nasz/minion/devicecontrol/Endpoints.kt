// Device control: port discovery (design 4.3). NSD records are hints only; the bridge dials 127.0.0.1 and nothing
// else, never scans ports.
// SPDX-License-Identifier: Apache-2.0
package cc.nasz.minion.devicecontrol

import android.content.Context
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import java.net.InetAddress
import java.net.NetworkInterface
import java.util.ArrayDeque
import java.util.concurrent.ConcurrentHashMap

/** One resolved `_adb-tls-*._tcp` record. */
data class AdbService(val name: String, val addresses: List<InetAddress>, val port: Int)

sealed class EndpointChoice {
    data class Port(val port: Int, val source: String) : EndpointChoice()
    object None : EndpointChoice()
    data class Ambiguous(val count: Int) : EndpointChoice()
}

object EndpointChooser {
    /**
     * The owner's manual port wins (it is an explicit choice). Otherwise exactly one NSD record whose resolved
     * address is one of this device's own addresses; records of other devices are ignored; several local
     * candidates with different ports are ambiguous and never auto-dialled.
     */
    fun choose(manualPort: Int?, services: Collection<AdbService>, own: Set<InetAddress>): EndpointChoice {
        if (manualPort != null && validPort(manualPort)) return EndpointChoice.Port(manualPort, "manual")
        val ports = services.filter { s -> validPort(s.port) && s.addresses.any { it in own } }.map { it.port }.toSet()
        return when (ports.size) {
            0 -> EndpointChoice.None
            1 -> EndpointChoice.Port(ports.first(), "nsd")
            else -> EndpointChoice.Ambiguous(ports.size)
        }
    }

    fun validPort(port: Int) = port in 1..65535

    /** Every address of every interface of this device (loopback included). */
    fun ownAddresses(): Set<InetAddress> = runCatching {
        NetworkInterface.getNetworkInterfaces()?.toList().orEmpty().flatMap { it.inetAddresses.toList() }.toSet()
    }.getOrDefault(emptySet())
}

/**
 * Watches one NSD service type and keeps the resolved records. [onChange] runs on an NSD thread. Resolves are
 * serialized (NsdManager allows one at a time on older releases).
 */
class NsdWatcher(context: Context, private val type: String, private val onChange: () -> Unit) {
    private val nsd = context.getSystemService(NsdManager::class.java)
    private val found = ConcurrentHashMap<String, AdbService>()
    private val queue = ArrayDeque<NsdServiceInfo>()
    private var resolving = false
    private var listener: NsdManager.DiscoveryListener? = null

    val services: Collection<AdbService> get() = found.values

    @Synchronized
    fun start() {
        if (listener != null || nsd == null) return
        val l = object : NsdManager.DiscoveryListener {
            override fun onDiscoveryStarted(serviceType: String) {}
            override fun onDiscoveryStopped(serviceType: String) {}
            override fun onStartDiscoveryFailed(serviceType: String, errorCode: Int) { synchronized(this@NsdWatcher) { listener = null } }
            override fun onStopDiscoveryFailed(serviceType: String, errorCode: Int) {}
            override fun onServiceFound(info: NsdServiceInfo) = enqueue(info)
            override fun onServiceLost(info: NsdServiceInfo) {
                if (found.remove(info.serviceName) != null) onChange()
            }
        }
        listener = l
        runCatching { nsd.discoverServices(type, NsdManager.PROTOCOL_DNS_SD, l) }.onFailure { listener = null }
    }

    @Synchronized
    fun stop() {
        listener?.let { l -> runCatching { nsd?.stopServiceDiscovery(l) } }
        listener = null
        queue.clear()
        if (found.isNotEmpty()) { found.clear(); onChange() }
    }

    private fun enqueue(info: NsdServiceInfo) {
        synchronized(this) { queue.add(info) }
        next()
    }

    @Suppress("DEPRECATION") // resolveService/host: still the API that works on 29..35.
    private fun next() {
        val info = synchronized(this) {
            if (resolving || listener == null) return
            queue.poll()?.also { resolving = true } ?: return
        }
        val done = {
            synchronized(this) { resolving = false }
            next()
        }
        runCatching {
            nsd!!.resolveService(info, object : NsdManager.ResolveListener {
                override fun onResolveFailed(serviceInfo: NsdServiceInfo, errorCode: Int) = done()
                override fun onServiceResolved(serviceInfo: NsdServiceInfo) {
                    val addresses = if (android.os.Build.VERSION.SDK_INT >= 34) serviceInfo.hostAddresses
                        else listOfNotNull(serviceInfo.host)
                    found[serviceInfo.serviceName] = AdbService(serviceInfo.serviceName, addresses, serviceInfo.port)
                    onChange()
                    done()
                }
            })
        }.onFailure { done() }
    }

    companion object {
        const val CONNECT = "_adb-tls-connect._tcp"
        const val PAIRING = "_adb-tls-pairing._tcp"
    }
}
