# netgear-plus-unifi

Experimental integration that makes NETGEAR Plus managed switches appear as
adopted, connected switches in UniFi Network, with port state and traffic
sourced live from the physical hardware. Read-only by default. Optionally,
port configuration made in UniFi (VLANs, native VLAN, disable, speed, name)
is applied to the switch, which UniFi then manages like a native switch
([docs/CONFIG-WRITE.md](docs/CONFIG-WRITE.md)).

Verified with a NETGEAR **GS308Ev4** (firmware V1.0.1.9) and **GS116Ev2**
(2.6.0.50) against a disposable UniFi Network **10.6.109** controller. See
[docs/REPORT.md](docs/REPORT.md) for what works, what does not, and why.

```
 NETGEAR GS308Ev4 ─┐  HTTP (web UI)   ┌──────────────────────────────────────┐  inform (TNBU,   ┌──────────────┐
 NETGEAR GS116Ev2 ─┴────────────────▶│ netgear-unifi (Go, one container)    │─────────────────▶│ UniFi Network│
                                      │  per switch: poller ── virtual UniFi │  AES-GCM) :8080  │ (DR7 / test) │
                                      │  switch; opt-in config sync          │◀─────────────────│              │
                                      └──────────────────────────────────────┘  port config     └──────────────┘
```

Per NETGEAR, one **poller** reads the switch (link, speed, duplex, 64-bit
byte counters, CRC errors), derives rates and detects counter resets, and,
if enabled, applies UniFi's port configuration between reads on the same
session. One **virtual UniFi switch** reports that data to the controller
and hands UniFi's configuration to the poller.

- `internal/netgear`: model detection, login, status pages and write
  protocols for the GS308Ev4 and GS116Ev2, reimplemented from
  [py-netgear-plus](https://github.com/foxey/py-netgear-plus) and the
  switches' own web-UI code.
- `internal/device`: the virtual UniFi switch on the inform protocol and
  adoption state machine of [unifi-emu](https://github.com/jamesbraid/unifi-emu)
  (`inform` package, unmodified), with live port tables and persisted
  adoption state.
- `internal/provision`: UniFi configuration parser, planner, verified writer
  and sync.

## Safety model

- By default nothing writes to a NETGEAR switch. Polling only GETs status
  pages (the only POSTs are login and logout), and a regression test fails
  if a read sends anything else.
- Port configuration writes are **opt-in per switch** and dry-run by default.
  Once live, UniFi manages every port of that switch. Every write is verified
  by read-back; anything the write did not name that changed is restored
  immediately. VLAN 1 and the management VLAN are never deleted. VLAN mode, management VLAN, IP, password, reboot,
  reset and firmware are not implemented at all. Details in
  [docs/CONFIG-WRITE.md](docs/CONFIG-WRITE.md).
- Other controller provisioning (firmware upgrades, reboots, everything
  outside port configuration) is acknowledged and never applied.
- Device identity: with `unifi.mac: physical` (as in the example config) the
  virtual switch uses the NETGEAR's own MAC, the address the gateway actually
  sees, so UniFi can place it in the topology. If the switch ever reports a
  different MAC, the device pauses instead of continuing under the old
  identity. `auto` derives a locally administered `02:4e:47:xx:xx:xx` address
  instead, which never appears on the wire.
- Credentials come from environment variables. Controller auth keys are
  stored 0600 and never logged. Captured controller messages are redacted.
- Without fresh telemetry the virtual switch stops informing, so UniFi shows the
  switch **offline** rather than stale ports.
- A virtual device stays bound to the physical switch (model and serial) it
  first saw, and refuses data from any other switch.

## Quick start (disposable test controller)

```sh
cp .env.example .env                                  # set NETGEAR_*_PASSWORD
cp config/switches.example.yaml config/switches.yaml  # adjust IPs if needed
export COMPOSE_FILE=docker-compose.yml:docker-compose.test.yml
docker compose up -d --build
go run ./cmd/unifi-testctl devices                    # wait until both are "pending"
go run ./cmd/unifi-testctl adopt 28:94:01:8f:ff:71 38:94:ed:0c:d4:9b
go run ./cmd/unifi-testctl ports 38:94:ed:0c:d4:9b
```

Test controller UI: <https://127.0.0.1:18443>, login `admin` /
`unifi-containers-seeded`. Device MACs per switch id: `docker compose exec
netgear-unifi netgear-unifi ids`. Full walkthrough: [docs/TESTING.md](docs/TESTING.md).
Without hardware, use `SWITCHES_CONFIG=./config/switches.mock.yaml`.

Production use alongside the Dream Router 7 requires explicit approval and is
described in [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).

## Configuration

One file, `config/switches.yaml` ([example](config/switches.example.yaml)).
Each switch has a `source` section (the physical NETGEAR) and a `unifi`
section (the virtual UniFi device). Unknown keys are errors.

| Setting | Meaning |
|---|---|
| `source.adapter` | `netgear_plus` or `mock` |
| `source.host`, `source.password_env` | switch address and the env var holding its password |
| `source.expected_model` | refuse data if another model answers at that address |
| `source.poll_interval_seconds` | default: top-level `poll_interval_seconds` (15) |
| `source.session_yield_seconds` | pause after the switch drops our session because someone logged into its web UI (default 300) |
| `source.write.enabled`, `.dry_run` | let UniFi manage the switch's ports (default off; dry run on) |
| `unifi.model` | UniFi model code: `USL8LP` for 8 ports, `USL16LP` for 16 ports (see report) |
| `unifi.mac` | `physical` (the NETGEAR's own MAC), `auto` (derived `02:4e:47:…`) or an explicit locally administered MAC. Changing it requires forgetting and re-adopting the device. |
| `unifi.ip` | `physical` (report the NETGEAR IP) or an IPv4 literal |
| `unifi.downlinks` | optional, **operator-declared** topology between configured switches; never detected |
| `controller.inform_url` | controller inform URL (an IP); env `UNIFI_INFORM_URL` overrides |
| `stale_after_seconds` | telemetry age after which informs pause (default 90) |
| `state_dir` | adoption state, contains auth keys (default `/data`; env `STATE_DIR`) |

## Operations

| Task | Command |
|---|---|
| Status: virtual device, switch telemetry, config sync | `curl -s 127.0.0.1:8098/status` |
| Forget adoption state of one switch | `docker compose exec netgear-unifi netgear-unifi reset gs308ev4`, then restart the service |
| Remove everything | remove the devices in UniFi (Settings → Devices → Forget), then `docker compose down -v` |
| Change a port in the test controller | `go run ./cmd/unifi-testctl port MAC 4 native=3901 tagged=none name=lab` |
| Debug logs (every switch request, redacted UniFi config) | `LOG_LEVEL=DEBUG` in `.env` |

## Development

```sh
go vet ./... && go test -race -cover ./...
```

The NETGEAR tests run against a fake switch HTTP server serving **real pages
recorded from both switches** (`internal/netgear/testdata`, serials, MACs
and session hashes anonymised); it emulates login, session cookies and
session loss. The device tests run the full CBC → key rotation → AES-GCM
adoption against an in-process fake controller.

### Adding a NETGEAR model

Only the GS308Ev4 and GS116Ev2 are supported. [AGENTS.md](AGENTS.md)
describes step by step how to add a model (recording pages, implementing
detection, login, status parsing and writes, tests, hardware verification)
or another vendor.

## Third-party code

- [unifi-emu](https://github.com/jamesbraid/unifi-emu) (MIT), used as a Go
  module dependency (`inform` package and model registry).
- [py-netgear-plus](https://github.com/foxey/py-netgear-plus) (Apache-2.0):
  the login hashing, model detection and status-page parsing for the two
  supported models were reimplemented in Go following it (commit `552b041`).
  No code is included.
- [golang.org/x/net/html](https://pkg.go.dev/golang.org/x/net/html) (BSD-3-Clause)
  and [gopkg.in/yaml.v3](https://github.com/go-yaml/yaml) (MIT/Apache-2.0).
- [unifi-stubd](https://github.com/konstruktor1/unifi-stubd) (AGPL-3.0) was
  evaluated and used only as a reference for payload field names. No code
  was copied.

Not affiliated with Ubiquiti or NETGEAR. Experimental; use on isolated or
non-critical networks first.
