# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- `netgear-unifi`: one self-contained Go program (one container) that
  presents NETGEAR GS308Ev4 and GS116Ev2 switches as adopted UniFi switches
  with live port data. Per switch, a poller reads the hardware and a virtual
  UniFi switch (unifi-emu's `inform` package, unmodified) reports it.
- NETGEAR support in Go (`internal/netgear`): model detection, login
  hashing, status pages and write protocols, reimplemented from
  py-netgear-plus and the switches' own web-UI code. Correct 64-bit counters
  on the GS308Ev4 (the library reads only the low word). Working logout on
  the GS116Ev2 (the library's `logout.cgi` does not exist there). GS308Ev4
  session cookies are handled verbatim (they contain characters strict
  cookie parsers drop).
- Telemetry: link state, negotiated speed, duplex (GS308Ev4), byte counters,
  CRC errors; rates derived from successive polls, counter resets detected,
  implausible samples dropped. Without fresh telemetry the virtual switch
  stops informing, so UniFi shows it offline.
- Detection of sessions displaced by a web-UI login on single-session
  switches (GS116Ev2), with a configurable yield instead of immediately
  logging the administrator out again.
- Opt-in UniFi → NETGEAR port configuration: with writes enabled for a
  switch, UniFi manages all its ports (VLAN create/delete, tagged/untagged
  membership, native VLAN (PVID), port disable, forced speed, port names)
  and the switch name (UniFi sends a hostname-safe form; at most 20
  characters; on the GS116Ev2 the IP settings sharing that form are echoed
  and verified). Dry run by default. Every write is verified by read-back,
  with automatic restore of any side effect. VLAN 1 and the management VLAN
  are never deleted. Drift is corrected every 5 minutes and after restarts.
  UniFi sees the device as provisioning until the hardware matches.
- Per-port `speed_caps` from each model's real speed options, so UniFi's
  port editor offers only settings the switch accepts. UniFi's forced
  1000 Mbit/s is applied as auto.
- `unifi.mac: physical`: the virtual device uses the NETGEAR's own MAC,
  learned from the switch and persisted, so the gateway sees the device's
  identity on the wire. A changed switch MAC pauses the device.
- Operator-declared downlink topology between configured switches.
- `unifi.uplink`: operator-declared upstream device (e.g. the gateway) and
  ports, reported as an LLDP neighbour, uplink object and `is_uplink`. UniFi
  marks the port as uplink and shows the gateway on it in the ports view; on a DR7 it still draws no topology line, because
  the controller builds wired topology from the upstream device's own LLDP.
- `unifi-testctl` logs in to UniFi OS consoles and is strictly read-only
  (GET only) on any non-test controller (`ALLOW_NON_TEST_CONTROLLER=1`); new
  `get PATH` command.
- Persisted adoption state (restart survival), binding of each virtual
  device to its physical switch (model and serial).
- Mock switches (link flapping, failure injection, in-memory writes) for
  development without hardware.
- Status endpoint (`127.0.0.1:8098/status`): virtual device, switch
  telemetry and config sync per switch. DEBUG logs of every switch request
  (no cookies or passwords) and of the redacted configuration UniFi sends.
- `cmd/unifi-testctl`: drives a disposable test controller (devices, ports,
  adopt, wait, forget, name, networks, port overrides); refuses non-test
  controllers.
- `docker-compose.coolify.yml` for Coolify (passwords as Coolify variables,
  config and state as bind mounts) and documented procedures for starting,
  restarting, moving the installation with its adoption state, and migrating
  to Coolify.
- Docker Compose deployment, disposable test-controller overlay,
  documentation (testing, DR7 deployment, technical report, configuration
  writes).
- `AGENTS.md` (also loaded via `CLAUDE.md`): project rules and a step-by-step
  guide for adding NETGEAR models or other vendors. Supported models are one
  list in `internal/netgear`; detection, session re-login and write support
  are per-model properties.

### Changed

- The Python collector (on py-netgear-plus) and the Go emulator were merged
  into one Go program; Python is no longer used. Configuration: the
  `collector` and `emulator` sections were replaced by `controller`
  (`inform_url`, `inform_interval_seconds`) and top-level
  `poll_interval_seconds`, `stale_after_seconds` and `state_dir`; the
  collector's internal API and `collector_url`/`telemetry_poll_seconds` are
  gone. Unknown keys are now errors. Existing adoption state is not
  migrated: forget the devices in UniFi and adopt them again.
- The Compose stack has one service (`netgear-unifi`) and one state volume;
  `ngu-emulator` is now `netgear-unifi`, `scripts/test_controller.py` is
  `go run ./cmd/unifi-testctl`.
- GS308Ev4 is presented as `USL8LP` (USW Lite 8 PoE) instead of `US8`,
  matching the GS116Ev2's USW Lite 16 PoE. Changing the model requires
  forgetting and re-adopting the device.
