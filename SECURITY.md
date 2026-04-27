# Security model: Yealink RPS, auto-provisioning, and the PnP escape hatch

This document describes how Yealink T4x phones decide *where to fetch their
configuration from* on first boot, why this matters when buying second-hand
phones, and how multicast SIP PnP can be used to take ownership of a phone
without involving Yealink at all.

## The intended flow: zero-touch provisioning via Yealink RPS

A factory-fresh Yealink phone ships with three things baked in:

1. A unique **device certificate** (X.509, `CN=<MAC>`, signed by the
   *Yealink Equipment Issuing CA* which chains to the *Yealink Root CA*).
2. Hardcoded **RPS URLs** (`https://rps.yealink.com` and regional variants).
3. A firmware-defined **discovery order** for finding its provisioning server.

The standard flow looks like this:

```
[Factory phone]                         [Yealink RPS]              [Operator DMS]
      |                                       |                          |
      | 1. boot, DHCP lease                   |                          |
      | 2. discovery sequence:                |                          |
      |    DHCP66 -> preconf -> PnP -> RPS    |                          |
      |                                       |                          |
      | 3. HTTPS to rps.yealink.com           |                          |
      |    + factory device cert (mTLS)      -|                          |
      |                                       |                          |
      |    4. RPS validates phone cert,       |                          |
      |       looks up MAC in registry,       |                          |
      |       returns assigned DMS URL        |                          |
      | <-------------------------------------|                          |
      |                                                                  |
      | 5. HTTPS to assigned URL + same device cert (mTLS)               |
      |---------------------------------------------------------------> |
      |                                                                  |
      |    6. DMS validates cert, extracts MAC, returns                  |
      |       y000000000044.cfg + <MAC>.cfg                              |
      | <----------------------------------------------------------------|
      |                                                                  |
      | 7. apply config, reboot if needed                                |
      | 8. on later triggers (timer / power-on / DTMF), repeat from 5   |
      |    using the now-stored static.auto_provision.server.url         |
```

Step 7 onwards is steady-state: the phone has its DMS URL stored locally and
the discovery sequence at every subsequent boot finds that URL in the
*preconfigured URL* slot before PnP or RPS are consulted again.

## The RPS "lock"

The RPS database is **authoritative** for which DMS URL a freshly-reset phone
gets sent to. This creates what is colloquially called the *RPS lock*:

- The RPS request happens over HTTPS to a hardcoded hostname.
- The phone validates the server cert against its bundled public-CA trust
  store, so a real cert for `rps.yealink.com` is needed to MITM it (i.e. you
  can't).
- The check is enforced in firmware before any user-supplied config is
  loaded, so you cannot disable it from outside the device.
- Changing the RPS registration requires a Yealink reseller account.

In other words: **if you only have access to the public-internet path the
phone takes, the lock holds**. A factory reset flushes any local config and
sends the phone right back to whichever DMS URL the previous owner's reseller
registered.

## Why this matters for resold / second-hand Yealink phones

Yealink phones are widely resold. When you buy used, you almost always
inherit the previous owner's RPS registration:

- The MAC in Yealink's RPS database still points at the old operator's DMS
  (e.g. `dms-tls.kpneen.nl`, `dms-tls.voipit.nl`, `dms-tls.proximus...`).
- Out of the box, the phone will phone home to that operator's DMS, present
  its factory device cert, and either:
  - get back the previous tenant's config (potentially including SIP
    credentials that no longer work, or worse, that *do* still work and
    register to a stranger's PBX), or
  - get a `404` / rejection because the cert is no longer mapped to a
    customer.
- Either way, **your phone is not provisioning from you**, and a factory
  reset puts it right back in that state.
- Without a Yealink reseller relationship you cannot get the registration
  cleared. Yealink does not offer a self-service *"I bought this phone,
  release it"* flow to end users.

Practical consequences for second-hand buyers:

| Scenario                                        | Result                                                 |
| ----------------------------------------------- | ------------------------------------------------------ |
| Plug phone in, expect it to be blank            | It tries to provision from previous operator's DMS     |
| Set a provisioning URL via the web UI manually  | Works, but a factory reset wipes it back to RPS        |
| Hope to MITM `rps.yealink.com` to redirect      | Cert pinning prevents this                             |
| Ask Yealink to release the phone                | Routed back to the original reseller; often no answer  |

## The escape hatch: multicast SIP PnP

The RPS lock only affects **step 3** of the discovery sequence. Step 2 — the
discovery sequence itself — runs first, and on every T4x firmware **PnP
fires before RPS by default**. A PnP responder on the phone's local network
can therefore hand the phone a provisioning URL before the phone ever
contacts Yealink.

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

### Why PnP defeats the lock

1. **PnP precedes RPS in the discovery order.** The phone tries each method
   until one yields a URL. PnP yields first, so the RPS code path simply does
   not execute. There is no lock to break because the lock is never tested.
2. **PnP has no authentication.** It is bare SIP over UDP. No TLS, no
   certificates, no shared secret, no signature on the response body.
   Whoever answers the multicast SUBSCRIBE first is believed.
3. **The trust model is *L2 access = ownership*.** This is intentional: PnP
   exists so enterprise PBX systems (FreePBX, 3CX, FusionPBX, Asterisk) can
   claim phones automatically on their own network. From the firmware's
   point of view, RPS is not privileged — it is just further down the list.
4. **It is reset-proof.** A factory reset wipes stored config but does not
   change the discovery-sequence behaviour, which is firmware. A reset phone
   on a LAN with a PnP responder goes through the same PnP-wins flow at the
   next boot.
5. **It is sticky.** Once your PnP responder hands the phone a URL, your DMS
   delivers a config that sets `static.auto_provision.server.url` directly.
   From then on the phone consults your URL via the *preconfigured URL* slot
   of the discovery sequence and never depends on PnP again.

So PnP does not *unlock* RPS — it routes around it. The lock is still there
in firmware; the discovery order simply renders it unreachable.

### What this means for resold phones

For a second-hand T4x with an unknown RPS registration history, multicast
PnP is the only fully-automatic way to reclaim the phone without a Yealink
reseller relationship. Plug the phone into a VLAN with a PnP responder on
it, factory-reset it, and at next boot it will provision from your server
instead of the previous operator's DMS. Subsequent factory resets behave the
same way as long as the responder remains on that L2 segment.

## Trade-offs and hardening

The same property that makes PnP useful makes it a risk in shared networks:

- **Anyone with L2 access** to a Yealink can hijack its provisioning at next
  boot by running their own PnP responder.
- The first responder to the multicast SUBSCRIBE wins. There is no
  tie-breaker, no preference for legitimate sources.
- For phones in untrusted environments (open offices, shared VLANs, guest
  networks), PnP should be disabled in the deployed config:

  ```
  static.auto_provision.pnp_enable = 0
  ```

  Once that setting is delivered, the phone stops sending the multicast
  SUBSCRIBE on boot, which closes the escape hatch.

- For self-hosted deployments, put phones on a **dedicated VLAN** with no
  untrusted hosts. L2 isolation is the actual security boundary; PnP is
  safe inside that boundary and dangerous outside it.

- Consider also blocking outbound traffic to `rps*.yealink.com` at the
  firewall as defence-in-depth: it prevents the phone from leaking its MAC
  to Yealink's RPS even if PnP fails for some reason.

## TL;DR

- Yealink's RPS is not a security control; it is a discovery convenience.
- The phone's discovery order puts local LAN methods (DHCP option 66, SIP
  PnP multicast) **above** RPS.
- PnP has no authentication, so anyone on the phone's L2 segment can claim
  it. This is the documented enterprise-PBX path.
- For resold phones with stale RPS registrations, PnP is the only
  self-service way to take ownership without going through a Yealink
  reseller.
- The trust boundary is the LAN, not the cert chain. Plan your network
  accordingly.
