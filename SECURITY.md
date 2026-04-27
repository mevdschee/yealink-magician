# Security model: Yealink RPS, auto-provisioning, and the PnP escape hatch

This document describes how Yealink T4x phones decide _where to fetch their
configuration from_ on first boot, why this matters when buying second-hand
phones, and how multicast SIP PnP can be used to take ownership of a phone
without involving Yealink at all.

The factual claims here are based on observed behaviour of T46S 66.86 and T43U
108.86 / 108.87 firmware. Older or newer firmware may differ — most notably the
default discovery order is configurable and has shifted across firmware
generations. Verify on the firmware revision you actually have before relying on
any specific detail below.

## The intended flow: zero-touch provisioning via Yealink RPS

A factory-fresh Yealink phone ships with three things baked in:

1. A unique **device certificate** (X.509, `CN=<MAC>`, signed by a
   Yealink-controlled CA chain).
2. **Hardcoded RPS hostnames**, set in firmware. The exact name has shifted
   across firmware generations (`dm.yealink.com`, `acs.yealink.com`,
   `rps.yealink.com` have all appeared); whichever a given phone uses is not
   user-configurable.
3. A firmware-defined **discovery order** for finding its provisioning server.

The standard flow looks like this:

```
[Factory phone]                         [Yealink RPS]              [Operator DMS]
      |                                       |                          |
      | 1. boot, DHCP lease                   |                          |
      | 2. discovery sequence (varies by      |                          |
      |    firmware; PnP and RPS both         |                          |
      |    appear, defaults differ)           |                          |
      |                                       |                          |
      | 3. HTTPS to the RPS hostname,         |                          |
      |    presenting factory device cert    -|                          |
      |                                       |                          |
      |    4. RPS validates phone cert,       |                          |
      |       looks up MAC in registry,       |                          |
      |       returns assigned DMS URL        |                          |
      | <-------------------------------------|                          |
      |                                                                  |
      | 5. HTTPS to assigned URL, presenting same device cert            |
      |    (server-side cert validation depends on operator's DMS)       |
      |---------------------------------------------------------------> |
      |                                                                  |
      |    6. DMS validates cert (or doesn't), extracts MAC, returns     |
      |       y000000000<model>.cfg + <MAC>.cfg                          |
      | <----------------------------------------------------------------|
      |                                                                  |
      | 7. apply config, reboot if needed                                |
      | 8. on later triggers (timer / power-on / DTMF), repeat from 5    |
      |    using the now-stored static.auto_provision.server.url         |
```

Step 7 onwards is steady-state: the phone has its DMS URL stored locally and the
discovery sequence at every subsequent boot finds that URL in the _preconfigured
URL_ slot before PnP or RPS are consulted again.

## The RPS "lock"

The RPS database is **authoritative** for which DMS URL a freshly-reset phone
gets sent to. This creates what is colloquially called the _RPS lock_:

- The RPS request happens over HTTPS to a hostname compiled into firmware.
- The phone validates the server cert against a Yealink-bundled trust anchor —
  _not_ the public-CA store. This is more restrictive than ordinary HTTPS
  validation: even a Let's Encrypt cert for the right hostname is rejected. On a
  factory-fresh phone with default trust settings, MITM is not a practical
  option.
- The check is enforced in firmware before any user-supplied config is loaded,
  so it cannot be disabled from outside the device without network/console
  access. (`static.security.trust_certificates = 0` and adding custom CAs both
  exist as cfg knobs, but applying them requires having already reached the
  phone via some other path.)
- Changing the RPS registration requires a Yealink reseller account.

In other words: **on the public-internet path, the lock holds for a
factory-fresh phone**. A factory reset flushes any local config and sends the
phone right back to whichever DMS URL the previous owner's reseller registered,
assuming such a registration exists.

## Why this matters for resold / second-hand Yealink phones

Yealink phones are widely resold. When you buy used, you often inherit the
previous owner's RPS registration — not always (small resellers and direct
retail channels don't always create RPS entries, and some operators do release
devices on account closure), but commonly enough to plan for:

- The MAC in Yealink's RPS database may still point at the old operator's DMS
  (e.g. `dms-tls.kpneen.nl`, `dms-tls.voipit.nl`, `dms-tls.proximus...`).
- Out of the box, the phone will phone home to that operator's DMS, present its
  factory device cert, and most commonly get back a `404` or rejection because
  the cert is no longer mapped to an active customer. On rare combinations (the
  previous customer record left active and still auto-provisioning to this MAC)
  the phone may receive a stale config; the practical visible effect is failed
  registrations on the screen.
- Either way, **your phone is not provisioning from you**, and a factory reset
  puts it right back in that state.
- Without a Yealink reseller relationship you cannot get the registration
  cleared. Yealink does not offer a self-service _"I bought this phone, release
  it"_ flow to end users.

Practical consequences for second-hand buyers:

| Scenario                                       | Result                                                                |
| ---------------------------------------------- | --------------------------------------------------------------------- |
| Plug phone in, expect it to be blank           | If RPS-registered, it tries to provision from previous operator's DMS |
| Set a provisioning URL via the web UI manually | Works, but a factory reset wipes it back to the discovery sequence    |
| Hope to MITM the RPS hostname to redirect      | Yealink-bundled trust anchor blocks this on factory-fresh phones      |
| Ask Yealink to release the phone               | Routed back to the original reseller; often no answer                 |

## The escape hatch: multicast SIP PnP

The RPS lock only affects **step 3** of the discovery sequence. Step 2 — the
discovery sequence itself — runs first, and on every T4x firmware tested here
(T46S 66.86, T43U 108.86, T43U 108.87) **PnP runs before RPS in the default
discovery order**. A PnP responder on the phone's local network can therefore
hand the phone a provisioning URL before the phone ever contacts Yealink. The
order is configurable via `static.auto_provision.boot_discovery.order` and
defaults have shifted across firmware generations — verify on the firmware
revision you actually have before relying on this.

```
[Factory phone]                          [LAN, multicast 224.0.1.75]
      |                                            |
      | 1. boot, DHCP lease                        |
      |                                            |
      | 2a. SIP SUBSCRIBE Event: ua-profile -----> |
      |     to sip:MAC@224.0.1.75:5060             |
      |     (UDP multicast, no TLS, no auth)       |
      |                                            |
      |                                            | --> received by
      |                                            |     PnP responder
      |                                            |     anywhere on L2
      |                                            |
      | 2b. SIP NOTIFY (unicast back to phone) <---|
      |     body: profile-push with provisioning URL
      |                                            |
      | 3. phone takes that URL as its provisioning server
      |    -> jumps directly to "step 5" against your DMS
      |    -> RPS is never queried
```

### Why PnP routes around the lock

1. **PnP precedes RPS in the discovery order on the firmware tested.** The phone
   tries each method until one yields a URL. PnP yields first, so the RPS code
   path simply does not execute. There is no lock to break because the lock is
   never tested.
2. **PnP NOTIFYs are unauthenticated in default Yealink config.** RFC 6080 has
   hooks for authenticated/encrypted profile delivery, but Yealink's default
   deployment accepts unsigned NOTIFYs over plain UDP — no TLS, no shared
   secret, no signature on the response body. Whoever answers the multicast
   SUBSCRIBE first is believed.
3. **The trust model is _L2 access = ownership_.** This is intentional: PnP
   exists so enterprise PBX systems (FreePBX, 3CX, FusionPBX, Asterisk) can
   claim phones automatically on their own network. From the firmware's point of
   view, RPS is not privileged — it is just further down the list.
4. **The path survives factory reset.** A reset wipes stored config but does not
   change the discovery-sequence behaviour, which is in firmware. A reset phone
   on a LAN with a PnP responder goes through the same PnP-wins flow at the next
   boot. The flip side is symmetric: a reset phone on a LAN with a _different_
   PnP responder follows that flow too — see "Trade-offs and hardening" below.
5. **It can be made sticky.** Once your PnP responder hands the phone a URL,
   your DMS delivers a config that sets `static.auto_provision.server.url`
   directly. From then on the phone consults your URL via the _preconfigured
   URL_ slot of the discovery sequence.

So PnP does not _unlock_ RPS — it routes around it. The lock is still there in
firmware; the discovery order simply renders it unreachable on the firmware
revisions tested.

### What this means for resold phones

For a second-hand T4x with an unknown RPS registration history, multicast PnP is
the most direct way to reclaim the phone without a Yealink reseller
relationship. Plug the phone into a VLAN with a PnP responder on it,
factory-reset it, and at next boot it will provision from your server instead of
the previous operator's DMS. Subsequent factory resets behave the same way as
long as the responder remains on that L2 segment.

## TL;DR

- Yealink's RPS is a security control with a documented exception — PnP runs
  first in the default discovery order, by design (so enterprise PBX systems can
  claim phones on their own network).
- On the firmware tested, the phone's default discovery order puts local LAN
  methods (DHCP option 66, SIP PnP multicast) **above** RPS. The order is
  firmware-configurable.
- PnP NOTIFYs are unauthenticated in default Yealink config, so anyone on the
  phone's L2 segment can claim it. This is the documented enterprise-PBX path.
- For resold phones with stale RPS registrations, PnP is the most direct
  self-service way to take ownership without going through a Yealink reseller.
