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
 NETGEAR GS308Ev4 ─┐  HTTP               ┌──────────────┐  JSON   ┌─────────────┐  inform (TNBU,  ┌──────────────┐
 NETGEAR GS116Ev2 ─┴──────────────────▶│  collector   │───────▶│  emulator   │────────────────▶│ UniFi Network│
                     py-netgear-plus    │  (Python)    │ :8099   │  (Go)       │  AES-GCM) :8080 │ (DR7 / test) │
                                        └──────────────┘         └─────────────┘                 └──────────────┘
                                          polls, rates,            one virtual UniFi switch
                                          counter resets,          per NETGEAR, persisted
                                          opt-in port config  ◀─── adoption state, forwards
                                                                   UniFi port configuration
```

- **collector/**: wraps [py-netgear-plus](https://github.com/foxey/py-netgear-plus)
  behind a session that can only fetch status pages. It normalizes
  link, speed, duplex and 64-bit byte counters into a vendor-independent model,
  derives rates, detects counter resets, and serves JSON on an internal port.
  If enabled, it translates UniFi port configuration into NETGEAR VLAN and
  port changes, using write protocols reverse-engineered
  from both switches' web UIs.
- **emulator/**: reuses the inform protocol and adoption state machine of
  [unifi-emu](https://github.com/jamesbraid/unifi-emu) (`inform` package,
  unmodified). It runs one virtual UniFi switch per NETGEAR, replaces the
  static port table with live telemetry, and persists adoption state.

## Safety model

- By default nothing writes to a NETGEAR switch. The polling adapter can
  reach only four status pages (all HTTP GET on both verified models; the
  only POSTs are login and logout), and a regression test fails if it
  references any library write path.
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
- Without fresh telemetry the emulator stops informing, so UniFi shows the
  switch **offline** rather than stale ports.
- A virtual device stays bound to the physical switch (model and serial) it
  first saw, and refuses data from any other switch.

## Quick start (disposable test controller)

```sh
cp .env.example .env                                  # set NETGEAR_*_PASSWORD
cp config/switches.example.yaml config/switches.yaml  # adjust IPs if needed
export COMPOSE_FILE=docker-compose.yml:docker-compose.test.yml
docker compose up -d --build
python3 scripts/test_controller.py devices            # wait until both are "pending"
python3 scripts/test_controller.py adopt 28:94:01:8f:ff:71 38:94:ed:0c:d4:9b
python3 scripts/test_controller.py ports 38:94:ed:0c:d4:9b
```

Test controller UI: <https://127.0.0.1:18443>, login `admin` /
`unifi-containers-seeded`. Device MACs per switch id: `docker compose exec
emulator ngu-emulator ids`. Full walkthrough: [docs/TESTING.md](docs/TESTING.md).
Without hardware, use `SWITCHES_CONFIG=./config/switches.mock.yaml`.

Production use alongside the Dream Router 7 requires explicit approval and is
described in [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).

## Configuration

One file, `config/switches.yaml`, shared by both services
([example](config/switches.example.yaml)). Each switch has a `source`
section (the physical NETGEAR, read by the collector) and a `unifi` section
(the virtual device, read by the emulator).

| Setting | Meaning |
|---|---|
| `source.adapter` | `netgear_plus` or `mock` |
| `source.host`, `source.password_env` | switch address and the env var holding its password |
| `source.expected_model` | refuse data if another model answers at that address |
| `source.poll_interval_seconds` | default 15 |
| `source.session_yield_seconds` | pause after the switch drops our session because someone logged into its web UI (default 300) |
| `source.write.enabled`, `.dry_run` | let UniFi manage the switch's ports (default off; dry run on) |
| `unifi.model` | UniFi model code: `USL8LP` for 8 ports, `USL16LP` for 16 ports (see report) |
| `unifi.mac` | `physical` (the NETGEAR's own MAC), `auto` (derived `02:4e:47:…`) or an explicit locally administered MAC. Changing it requires forgetting and re-adopting the device. |
| `unifi.ip` | `physical` (report the NETGEAR IP) or an IPv4 literal |
| `unifi.downlinks` | optional, **operator-declared** topology between configured switches; never detected |
| `emulator.inform_url` | controller inform URL; env `UNIFI_INFORM_URL` overrides |
| `emulator.stale_after_seconds` | telemetry age after which informs pause (default 90) |

## Operations

| Task | Command |
|---|---|
| Status of virtual devices | `curl -s 127.0.0.1:8098/status` |
| Telemetry from the collector | `docker compose exec collector python -c "import urllib.request;print(urllib.request.urlopen('http://127.0.0.1:8099/v1/switches').read().decode())"` |
| Forget adoption state of one switch | `docker compose exec emulator ngu-emulator reset gs308ev4`, then restart the emulator |
| Remove everything | remove the devices in UniFi (Settings → Devices → Forget), then `docker compose down -v` |
| Port configuration sync status | `config_sync` in the collector's `/v1/switches` output (see [CONFIG-WRITE.md](docs/CONFIG-WRITE.md)) |
| Change a port in the test controller | `python3 scripts/test_controller.py port MAC 4 native=3901 tagged=none name=lab` |

## Development

```sh
cd collector && python3 -m venv .venv && .venv/bin/pip install -e '.[dev]'
.venv/bin/pytest --cov && .venv/bin/ruff check src tests && .venv/bin/ruff format --check src tests
cd ../emulator && go vet ./... && go test -cover ./...
```

The collector's adapter tests replay **real pages recorded from both
switches** (`collector/tests/fixtures`, serials, MACs and session hashes
anonymised). The emulator's lifecycle tests run the full CBC → key rotation
→ AES-GCM adoption against an in-process fake controller.

### Adding a NETGEAR model

1. If py-netgear-plus supports it, enable it with `adapter: netgear_plus`
   and set `expected_model`. Unverified models log a warning.
2. Record its three status pages, add them under `collector/tests/fixtures/`,
   and extend `tests/conftest.py::FakeSession.PAGES`.
3. Check the counter format. The library reads only the low 32-bit word on
   the GS108Ev4 family, which is fixed here in `adapters/netgear_pages.py`.
4. Choose a UniFi model with the same port count and set `unifi.model`.
   PoE is always reported as absent.

## Third-party code

- [unifi-emu](https://github.com/jamesbraid/unifi-emu) (MIT), used as a Go
  module dependency (`inform` package and model registry).
- [py-netgear-plus](https://github.com/foxey/py-netgear-plus) (Apache-2.0),
  pinned to commit `552b041`.
- [unifi-stubd](https://github.com/konstruktor1/unifi-stubd) (AGPL-3.0) was
  evaluated and used only as a reference for payload field names. No code
  was copied.

Not affiliated with Ubiquiti or NETGEAR. Experimental; use on isolated or
non-critical networks first.
