# configs

Yealink `.cfg` files to feed to the magician via `-cfg`.

## empty.cfg

A neutral "blank slate" config. Loading it leaves the phone with no SIP account,
no provisioning server, and no management server pointing anywhere real — useful
when you want to wipe a phone back to a clean, unconfigured state without
leaving it tethered to whatever provisioning URL its previous owner had set.

What it does:

- Resets the `user` and `admin` web/UI passwords to their defaults
  (`user`/`user` and `admin`/`admin`).
- Disables the management server (`static.managementserver.enable = 0`) and
  points its URL at `127.0.0.1` so any stale config can't phone home.
- Disables auto-provisioning (`static.auto_provision.mode = 0`) and blanks every
  `static.autoprovision.{32..41}` slot back to localhost.

Usage:

```
go run ./cmd/magician -ip <phone-ip> -cfg configs/empty.cfg
```
