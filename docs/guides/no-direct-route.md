# A Worker with no direct route to the Hub

Back to the [README](../../README.md).

A Worker only ever connects **out** to the Hub; the Hub never connects to a Worker. A Worker behind NAT or a firewall that allows outgoing connections needs nothing special. This guide is for the case where the Worker cannot reach the Hub's address at all, for example the Hub lives on a private network and the Worker is elsewhere, and a machine you can SSH to can bridge the gap. An SSH tunnel is plain transport here: the Worker still pins the Hub's identity key when it pairs or enrolls, so the tunnel and its certificate are not what the Worker trusts.

## The Hub is on a private network, the Worker is outside

A jump host that can reach the Hub (`hub.internal.example:8443`) and that you can SSH into (`jump.example.com`). On the **Worker machine**, open a local forward and keep it open:

```sh
ssh -N \
  -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
  -L 127.0.0.1:18443:hub.internal.example:8443 \
  worker-tunnel@jump.example.com
```

Point the Worker at the forwarded port:

```sh
minion worker pair --server https://127.0.0.1:18443
```

Use the same address (`https://127.0.0.1:18443`) for `minion worker join`, `minion worker up --server ...` and the Worker service, because that is the address the Worker connects to from then on. The Hub's TLS certificate is not checked for this connection, so the certificate not matching `127.0.0.1` does not matter.

## The Hub is private and cannot be reached by the jump host

If only the Hub's own machine can open connections, and the Worker's network can reach a relay host (`relay.example.com`), use a **remote forward** from the Hub machine and a local forward on the Worker machine:

```sh
# On the Hub machine: publish the Hub on the relay's loopback interface
ssh -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
  -R 127.0.0.1:18443:127.0.0.1:8443 hub-tunnel@relay.example.com

# On the Worker machine: reach that relay port
ssh -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
  -L 127.0.0.1:18443:127.0.0.1:18443 worker-tunnel@relay.example.com

minion worker pair --server https://127.0.0.1:18443
```

The two commands use two different accounts on the relay, one for each direction ([limit what the tunnel accounts can do](#limit-what-the-tunnel-accounts-can-do)).

The Hub must listen on `127.0.0.1:8443` or an address the Hub machine can reach (`MINION_LISTEN`); with the Hub's own certificate (`MINION_TLS=auto`, the default) nothing else is needed.

## Keeping the tunnel up

A tunnel that drops disconnects the Worker, which then reconnects by itself when the tunnel is back. Run the `ssh` command under a supervisor, for example as a systemd service with `Restart=always`, or with a tool such as `autossh`. The options above make `ssh` exit when the forward cannot be set up or the connection dies, so the supervisor restarts it.

## Limit what the tunnel accounts can do

Give each tunnel direction its own account on the jump host or relay, and configure the account in `sshd_config` on that host. The key options in `authorized_keys` are not enough on their own:

- `restrict` turns the optional key features off (terminal, agent and X11 forwarding, port forwarding), but it does **not** prevent a command or a shell without a terminal. A key with `restrict` can still run `ssh host command` as that account.
- `port-forwarding` switches both forwarding directions back on. `permitopen` then limits only **local** forwards (`ssh -L`) and `permitlisten` limits only **remote** forwards (`ssh -R`); with only one of them set, the other direction is open to any destination ([`permitopen` and `permitlisten` in sshd(8)](https://man.openbsd.org/sshd.8#permitopen)).

What stops commands is `MaxSessions 0`: it refuses every shell, login and subsystem session on a connection while still permitting forwarding ([`MaxSessions` in sshd_config(5)](https://man.openbsd.org/sshd_config.5#MaxSessions)). `ssh -N` opens no session, so the tunnel is not affected. `AllowStreamLocalForwarding no` refuses Unix-domain-socket forwards (`ssh -L /path:/path`, `ssh -R /path:...`), which are authorized separately from TCP and default to yes ([`AllowStreamLocalForwarding` in sshd_config(5)](https://man.openbsd.org/sshd_config.5#AllowStreamLocalForwarding)). `AllowTcpForwarding local` or `remote` allows only that one direction, `PermitOpen` and `PermitListen` name the only destinations or listen addresses, and `GatewayPorts no` (the default) keeps a remote forward on the relay's loopback interface ([`AllowTcpForwarding`, `PermitOpen`, `PermitListen` and `GatewayPorts` in sshd_config(5)](https://man.openbsd.org/sshd_config.5#AllowTcpForwarding)).

Create the accounts without a password and with a non-login shell, then add this to `/etc/ssh/sshd_config` of the jump host (`worker-tunnel`, for the local forward of the first section) or of the relay (both accounts, for the second):

```text
# Worker machine: local forward (ssh -L) to the Hub, nothing else.
Match User worker-tunnel
    MaxSessions 0
    AllowTcpForwarding local
    AllowStreamLocalForwarding no
    PermitOpen hub.internal.example:8443
    PermitTTY no
    AllowAgentForwarding no
    X11Forwarding no
    PermitTunnel no
    ForceCommand /usr/sbin/nologin

# Hub machine, relay case: remote forward (ssh -R) of one loopback port, nothing else.
Match User hub-tunnel
    MaxSessions 0
    AllowTcpForwarding remote
    AllowStreamLocalForwarding no
    PermitListen 127.0.0.1:18443
    GatewayPorts no
    PermitTTY no
    AllowAgentForwarding no
    X11Forwarding no
    PermitTunnel no
    ForceCommand /usr/sbin/nologin
```

On the relay the Worker's account `worker-tunnel` is the same kind of block as above with `PermitOpen 127.0.0.1:18443` instead. Allow only key logins for these accounts (`PasswordAuthentication no` for the host, or `AuthenticationMethods publickey` in the block).

Then also narrow each key in that account's `~/.ssh/authorized_keys`; this is a second line of defense, not the first:

```text
# worker-tunnel
restrict,port-forwarding,permitopen="hub.internal.example:8443" ssh-ed25519 AAAA... worker-tunnel
# hub-tunnel
restrict,port-forwarding,permitlisten="127.0.0.1:18443" ssh-ed25519 AAAA... hub-tunnel
```

Check the result on the jump host or relay before you rely on it. `sudo sshd -t` checks the syntax, and `sudo sshd -T -C user=worker-tunnel,host=client.example.com,addr=203.0.113.7` prints the settings that apply to that account (look for `maxsessions 0`, `allowtcpforwarding local`, `allowstreamlocalforwarding no`, `permitopen` and `permitlisten`). From the client, `ssh worker-tunnel@jump.example.com true` must be refused, and a `-L` to any other destination, or an `-R` at all, must fail. These checks are yours to run: the project's tests do not run an SSH server.

## Reaching the web UI the same way

A browser on a machine with no route can use a local forward too (`ssh -N -L 127.0.0.1:8443:hub.internal.example:8443 worker-tunnel@jump.example.com`, then `https://127.0.0.1:8443`). The browser will warn about the Hub's self-signed certificate; compare its fingerprint with the one in the Hub's log before you accept it. Sign-in with an identity provider ([OIDC](oidc.md)) redirects the browser to the Hub's address, which has to be registered at the identity provider; an address like `https://127.0.0.1:8443` is rarely what you want to register there, so give such a Hub a name behind a [reverse proxy](reverse-proxy.md) for that.
