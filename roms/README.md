# Yealink T4x firmware

Drop `.rom` files in this directory. The magician serves them via
`-firmware <path>`.

## Where to get them

Yealink hosts firmware on the support portal, organized by product
family. One rom is shared across all models in a family.

| Series | Models | Firmware family | Portal page |
|---|---|---|---|
| T4xS | T46S, T48S, T42S, T41S, T40G, T40P | 66.x | [T4xS firmware list](https://support.yealink.com/en/portal/docList?archiveType=software&productCode=809bb158e075168e) |
| T4xU | T43U, T46U, T48U, T42U, T44U, T44W | 108.x | [T4xU firmware list](https://support.yealink.com/en/portal/docList?archiveType=software&productCode=3db24770a40346d3) |

Latest as of April 2026:

- **T4xS:** [66.86.0.15](https://support.yealink.com/document-detail/a9b9511c3d6e4df28ba09d88061630d3)
- **T4xU:** [108.87.0.20](https://support.yealink.com/document-detail/d9f5c6f76b4d41dcb17a9cd607fe33da)

The portal pages list the full version history with release notes; the
"latest" links above pin specific releases. Check the portal for newer
versions before flashing fleet hardware.

## Filename convention

The rom filename advertises every model the file applies to, in parens:

```
T46S(T48S,T42S,T41S)-66.86.0.15.rom
T46U(T43U,T46U,T41U,T48U,T42U,T44U,T44W)-108.87.0.20.rom
```

The phone validates the firmware blob's *internal* model/version header
on flash, not the filename, so you can rename the file freely. The
magician serves it at a fixed `/firmware.rom` path regardless.

## Upgrade-path gotchas

- **T4xU V87 is a one-way bridge.** Once a phone is on 108.87+, the
  firmware refuses to downgrade to anything prior to V87. Decide before
  you flash whether you might need to roll back.
- **T4xS step-upgrades.** Some T46S units fail to jump directly to the
  latest from much older firmware; an intermediate hop (e.g. 66.85.0.5)
  has been reported to clear the path. If `-firmware` lands and the
  phone refuses without an obvious reason, try a stepping rom first.
