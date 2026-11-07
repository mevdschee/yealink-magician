# yealink-magician

Push a config onto a Yealink desk phone (T4X series) without having its admin
credentials, by riding the multicast PnP handshake the phone does on boot after
a factory reset.

Tested on T46S and T43U.

## How it works

A fresh-from-factory Yealink, on boot, multicasts a SIP `SUBSCRIBE` for the
`ua-profile` event to `224.0.1.75:5060`. Anything on the same L2 segment that
replies with a `NOTIFY` carrying a provisioning URL will be obeyed — no
credentials required. (RFC 6080 / draft-ietf-sipping-config-framework.)

The magician is the orchestrator around that handshake. For more details read
[SECURITY.md](SECURITY.md).

## Usage

```
go run ./cmd/magician -cfg <path-to-cfg> [-ip <phone-ip>] [-firmware <path-to-rom>]
```

Flags:

| Flag         | Default    | Notes                                     |
| ------------ | ---------- | ----------------------------------------- |
| `-cfg`       | (required) | Path to the `.cfg` to push.               |
| `-ip`        | (optional) | Phone's IPv4 address; also the filter.    |
| `-firmware`  | (optional) | Path to a `.rom` firmware to push.        |
| `-interface` | auto       | Network interface to bind.                |
| `-http-port` | `25565`    | Embedded HTTP server port.                |
| `-timeout`   | `10m`      | How long to keep running.                 |
| `-yes`       | `false`    | Answer every SUBSCRIBE without prompting. |

Details:

- **`-ip`** is optional. When given, it is also the IP filter — SUBSCRIBES and
  HTTP fetches from any other address on the segment are ignored, so two phones
  resetting at once won't cross-talk, and the bind interface is routed toward
  that address. When omitted, the magician answers any phone on the segment
  (still gated by the per-SUBSCRIBE prompt) and binds the first usable
  interface, or the one named by `-interface`. If the phone's DHCP lease flips
  during the reset while a filter is set, the SUBSCRIBE arrives from an
  unexpected address and you'll see `ignoring SUBSCRIBE from … (want …)`;
  re-run with the new IP or drop `-ip`.
- **Per-SUBSCRIBE prompt.** For every SUBSCRIBE that passes the filter the
  magician asks `send provisioning URL to <addr>? [y/N]` on the terminal and
  only replies with a NOTIFY if you answer `y`. Pass `-yes` to answer them all
  automatically (scripted or headless runs). With no TTY and without `-yes`,
  SUBSCRIBEs are skipped.
- **It keeps running.** The magician no longer exits after the first cfg
  fetch. It stays up, serving the cfg and firmware and prompting on each
  SUBSCRIBE, until you Ctrl-C it or `-timeout` elapses, so you can provision
  several phones in one session.
- **`-cfg`** bytes are served verbatim at `/y000000000000.cfg`. The one
  exception is when `-firmware` is set, in which case a `static.firmware.url`
  line is appended pointing at the magician's own HTTP server.
- **`-firmware`**, when set, is served at `/firmware.rom` with full Range
  support (the phone pulls in chunks). The phone validates the rom's internal
  model/version header, so the on-disk filename is irrelevant. If the rom's
  version equals what's already flashed, the phone silently no-ops the reflash.
- **`-interface`** is auto-detected from the route to `-ip` when `-ip` is set;
  otherwise the first usable IPv4 interface is used. Override only if you have
  multiple NICs on the same L2 segment and the wrong one is winning the route
  lookup. Whatever interface is chosen is also where the PnP multicast group
  (`224.0.1.75`) is joined.
- **`-http-port`** is where the cfg and firmware are served. Must be reachable
  from the phone (TCP inbound on the host firewall). The default `25565` is
  arbitrary — pick anything free.
- **`-timeout`** is the upper bound on how long the magician stays up. Bump it
  for long sessions or slow firmware pulls over constrained links.

What happens, in order:

1. Picks the bind interface (routed toward `-ip` if set, else the first usable
   one or `-interface`) and binds the HTTP listener on it. The cfg is served at
   `/y000000000000.cfg`; firmware, if given, at `/firmware.rom` (with full
   Range support).
2. Joins the PnP multicast group on the same interface.
3. Prompts you to factory-reset the phone (hold OK ~10s, confirm).
4. When a SUBSCRIBE arrives (from the target IP, if `-ip` is set), it prints
   the subscriber and asks `send provisioning URL to <addr>? [y/N]`. Answer `y`
   (or pass `-yes`) and it replies with a NOTIFY pointing at the cfg URL.
5. The phone GETs the cfg (IP-filtered), applies it, and, if `-firmware` was
   passed, GETs `/firmware.rom` and reflashes.
6. The magician stays up, ready for the next SUBSCRIBE, until you Ctrl-C it or
   `-timeout` elapses.

### Verified flow (cfg only)

```
HTTP serving cfg (1547 bytes) at http://192.168.178.33:25565/y000000000000.cfg
PnP listening on 224.0.1.75:5060 (interface=enp108s0)
IP filter: answering only 192.168.178.162

==> Now factory-reset the phone at 192.168.178.162.
    Press and hold the OK key for ~10s, then confirm the reset prompt.

Running until Ctrl-C or 10m0s elapses; you'll be prompted on each SUBSCRIBE.

PnP: SUBSCRIBE from 192.168.178.162:5059 (Yealink SIP-T46S 66.86.0.15)
    send provisioning URL to 192.168.178.162:5059 (Yealink SIP-T46S 66.86.0.15)? [y/N] y
PnP: sending URL to 192.168.178.162:5059 -> http://192.168.178.33:25565/y000000000000.cfg
notified 192.168.178.162:5059 -> http://192.168.178.33:25565/y000000000000.cfg
HTTP: GET /y000000000000.cfg from 192.168.178.162:57794 (Yealink SIP-T46S 66.86.0.15)
^C
shutting down
```

## Operational requirements

- **Same L2 segment.** Multicast doesn't cross VLANs/subnets without IGMP relay.
  The magician must be on the same broadcast domain as the phone.
- **UDP/5060 inbound on the host firewall.** ufw / firewalld will silently drop
  the multicast SUBSCRIBE before it reaches our socket — IGMP join succeeds, but
  the packet never gets delivered. Punch a hole:
  ```
  sudo ufw allow from <phone-subnet>/24 to any port 5060 proto udp
  ```
- **TCP/`-http-port` inbound** (default 25565) for the cfg fetch.
- **No HTTPS / no cert pinning.** Plain HTTP only.

## Gotchas

- **Sticky provisioning URLs.** Some prior-owner configurations survive a
  factory reset on this firmware. If the phone fetches from a remembered URL,
  gets "no version change," and shows `update skipped` on screen, it may not
  bother with PnP at all on subsequent boots. Capture with
  `tcpdump -i <iface> 'host <phone-ip> and (port 53 or port 80 or port 443
  or port 5060)'`
  during a reset to see what URL the phone is hitting, then either block it
  network-side or impersonate it.
- **"update skipped" with no PnP traffic.** Almost always means a host firewall
  is dropping multicast inbound, not that the phone failed to send. Check
  `cat /proc/net/igmp` to confirm the join is registered, then check ufw /
  firewalld.
- **The IP filter assumes a stable DHCP lease.** If the phone's IP changes
  during the reset, the SUBSCRIBE will arrive from an unexpected address and the
  magician will log "ignoring SUBSCRIBE from … (want …)" rather than responding.
  Re-run with the new IP.
- **PnP must be enabled and run before RPS.** PnP fires once per boot when
  `static.auto_provision.pnp_enable = 1` and the default
  `static.auto_provision.boot_discovery.order` puts PnP ahead of RPS. Both are
  factory defaults, but a sticky config from a prior owner can have changed
  either, in which case this tool can't reach the phone. The same applies in
  reverse: if the cfg you push sets `pnp_enable = 0` (or reorders discovery),
  this becomes a one-shot — no PnP path back in afterwards.

## Layout

```
cmd/magician/main.go          # CLI orchestration: HTTP server + IP filter
internal/pnp/responder.go     # Multicast SIP SUBSCRIBE/NOTIFY plumbing
```

## Build

```
go build ./...
```

Go 1.25+. No external dependencies.
