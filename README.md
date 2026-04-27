# yealink-magician

Push a config onto a Yealink desk phone (T46S firmware family 66.x) without its
admin credentials, by riding the multicast PnP handshake the phone does on boot
after a factory reset.

## How it works

A fresh-from-factory Yealink, on boot, multicasts a SIP `SUBSCRIBE` for the
`ua-profile` event to `224.0.1.75:5060`. Anything on the same L2 segment that
replies with a `NOTIFY` carrying a provisioning URL will be obeyed — no
credentials required. (RFC 6080 / draft-ietf-sipping-config-framework.)

The magician is the orchestrator around that handshake.

## Usage

```
go run ./cmd/magician -ip <phone-ip> -cfg <path-to-cfg> [-firmware <path-to-rom>]
```

Flags:

| Flag         | Default    | Notes                                                                                                                                                                                               |
| ------------ | ---------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-ip`        | (required) | Phone's current IPv4 address. Used as a filter — SUBSCRIBES from other devices on the segment are ignored.                                                                                          |
| `-cfg`       | (required) | Path to the `.cfg` file to push. Bytes are served verbatim, except a `static.firmware.url` line is appended when `-firmware` is set.                                                                |
| `-firmware`  | (optional) | Path to a `.rom` firmware to push. When set, the magician injects `static.firmware.url` into the cfg pointing at its own HTTP server, and waits for the phone to fetch the firmware before exiting. |
| `-interface` | auto       | Network interface to bind. Auto-detected from the route to `-ip`.                                                                                                                                   |
| `-http-port` | `25565`    | Port for the embedded HTTP server.                                                                                                                                                                  |
| `-timeout`   | `10m`      | Total time to wait for the SUBSCRIBE + cfg fetch + firmware fetch.                                                                                                                                  |

What happens, in order:

1. Resolves which interface routes to the phone and binds the HTTP listener on
   that interface. The cfg is served at `/y000000000000.cfg`; firmware, if
   given, at `/firmware.rom` (with full Range support).
2. Joins the PnP multicast group on the same interface.
3. Prompts you to factory-reset the phone (hold OK ~10s, confirm).
4. When a SUBSCRIBE arrives **from the target IP**, replies with a NOTIFY
   pointing at the cfg URL.
5. Waits for the phone to GET the cfg (IP-filtered).
6. If `-firmware` was passed, waits for the phone to GET `/firmware.rom`, then
   stays up an extra 30s in case the phone is still pulling ranges.
7. Exits. The phone applies the cfg, reflashes if firmware was supplied, and
   reboots.

The phone validates the firmware blob's internal model/version header, so the
rom filename on disk doesn't have to match anything specific — the magician
serves it at a fixed `/firmware.rom` path regardless. If the rom's version
equals what's already flashed, the phone silently no-ops the reflash.

### Verified flow (cfg only)

```
HTTP serving cfg (1547 bytes) at http://192.168.178.33:25565/y000000000000.cfg
PnP listening on 224.0.1.75:5060 (interface=enp108s0)

==> Now factory-reset the phone at 192.168.178.162.
    Press and hold the OK key for ~10s, then confirm the reset prompt.

PnP: SUBSCRIBE from 192.168.178.162:5059 (Yealink SIP-T46S 66.86.0.15) — sending URL
notified 192.168.178.162:5059 -> http://192.168.178.33:25565/y000000000000.cfg
HTTP: GET /y000000000000.cfg from 192.168.178.162:57794 (Yealink SIP-T46S 66.86.0.15)
done — phone will apply config and reboot
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
- **First boot after reset only.** PnP fires once per boot when
  `static.auto_provision.pnp_enable = 1`. If the cfg you push sets
  `pnp_enable = 0`, this is a one-shot tool — there is no PnP path back into
  that phone afterwards.

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
