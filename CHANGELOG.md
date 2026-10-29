# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- `unifi.mac: physical`: the virtual UniFi device uses the NETGEAR's own MAC,
  learned from the collector and persisted, so the gateway sees the device's
  identity on the wire and UniFi can place it in the topology. A changed switch
  MAC pauses the device. Declared downlinks resolve peer MACs at inform time.

- Device name from UniFi applied as the NETGEAR switch name (UniFi sends a
  hostname-safe form; at most 20 characters). On the GS116Ev2 the name shares
  a form with the management IP settings, which are echoed unchanged and
  verified.
- The collector reports each switch's hardware MAC again.
- DEBUG log of the (redacted) `system_cfg` received from UniFi.

- Per-port `speed_caps` reported to UniFi from the NETGEAR model's real speed
  options, so UniFi's port editor offers only settings the switch accepts
  (e.g. no half duplex on the GS308Ev4). UniFi's forced 1000 Mbit/s is applied
  as auto.

- Opt-in UniFi → NETGEAR port configuration: with writes enabled for a
  switch, UniFi manages all its ports (VLAN create/delete, tagged/untagged
  membership, native VLAN (PVID), port disable, forced speed, port names).
  Covers the GS308Ev4 (GS30x protocol) and the GS116Ev2 (reverse-engineered
  JGS form protocol). Dry run by default. Every write is verified by
  read-back, with automatic restore of any side effect. VLAN 1 and the
  management VLAN are never deleted. Drift is corrected. UniFi sees the device as provisioning
  until the hardware matches (cfgversion acknowledged only after apply).
- `scripts/test_controller.py`: `network`, `network-delete`, `port` and
  `port-reset` commands for driving port configuration on a test controller.

- Collector (Python): NETGEAR Plus adapter on py-netgear-plus
  (pinned `552b041`). It reports model, firmware, serial, link state,
  negotiated speed, duplex (GS308Ev4), 64-bit byte counters and CRC errors.
  Rates are derived from successive polls, counter resets are detected, and
  implausible samples are dropped.
- Correct 64-bit counters for the GS108Ev4/GS308Ev4 family. The library reads
  only the low word.
- Detection of sessions displaced by a web-UI login on single-session
  switches (GS116Ev2), with a configurable yield instead of immediately
  logging the administrator out again.
- Mock adapter (GS308Ev4/GS116Ev2 simulation with link flapping and failure
  injection) for development without hardware.
- Emulator (Go): virtual UniFi switches on unifi-emu's `inform` package. It
  reports live port tables, persists adoption state (restart survival), binds
  each virtual device to its physical switch, pauses informs on stale
  telemetry, and never applies controller provisioning outside port
  configuration. Operator-declared downlink topology between configured
  switches.
- Docker Compose deployment, disposable test-controller overlay, mock
  configuration, test-controller helper script, documentation (testing, DR7
  deployment, technical report, configuration-write proposal).

### Changed

- GS308Ev4 is presented as `USL8LP` (USW Lite 8 PoE) instead of `US8`, matching
  the GS116Ev2's USW Lite 16 PoE. Changing the model requires forgetting and
  re-adopting the device.
