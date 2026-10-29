# Testing against a disposable UniFi controller

Everything here runs on one Docker host on the same LAN as the switches.
The test controller is a throwaway container and never touches the Dream
Router. Unless you enable `source.write`, the only traffic to the switches
is the collector's read-only HTTP polling.

## 1. Automated tests (no hardware, no controller)

```sh
cd collector && python3 -m venv .venv && .venv/bin/pip install -e '.[dev]'
.venv/bin/pytest --cov
cd ../emulator && go test -cover ./...
```

## 2. Configure

```sh
cp .env.example .env            # NETGEAR_GS308EV4_PASSWORD=..., NETGEAR_GS116EV2_PASSWORD=...
chmod 600 .env
cp config/switches.example.yaml config/switches.yaml
```

The example already points at `192.168.1.247` (GS308Ev4) and `192.168.1.230`
(GS116Ev2). Leave `emulator.inform_url` alone: the test overlay overrides it.

## 3. Start collector, emulator and the test controller

```sh
export COMPOSE_FILE=docker-compose.yml:docker-compose.test.yml
docker compose up -d --build
docker compose logs -f collector emulator   # Ctrl-C when "pending adoption" appears
```

The controller needs 1–3 minutes to boot (the emulator logs HTTP 503 until
it is ready). Expected log lines:

```
collector  ... logged in switch=gs308ev4 host=192.168.1.247 model=GS308Ev4
emulator   ... bound to physical switch switch=gs308ev4 ... physical_model=GS308Ev4 firmware=V1.0.1.9
emulator   ... pending adoption: waiting for Adopt in UniFi Network switch=gs308ev4 mac=28:94:01:8f:ff:71
```

## 4. Adopt

Either open <https://127.0.0.1:18443> (accept the self-signed certificate;
login `admin` / `unifi-containers-seeded`) → UniFi Devices → *Adopt* on both
switches, or:

```sh
python3 scripts/test_controller.py devices
python3 scripts/test_controller.py adopt 28:94:01:8f:ff:71 38:94:ed:0c:d4:9b
python3 scripts/test_controller.py wait  28:94:01:8f:ff:71 38:94:ed:0c:d4:9b
```

The script refuses any controller outside loopback and Docker networks, so
it cannot be pointed at the DR7 by accident.

## 5. Verify the success criterion

```sh
python3 scripts/test_controller.py devices
python3 scripts/test_controller.py ports 38:94:ed:0c:d4:9b
```

Compare with the NETGEAR web UI (or unplug a cable):

- link up/down per port matches the switch;
- `speed` is the negotiated speed (10/100/1000);
- `rx`/`tx` are the switch's byte counters and `rx/s`/`tx/s` change between
  runs;
- `poe_caps=0` everywhere.

A link change shows up in UniFi within one poll interval (15 s) plus one
inform interval (10 s).

## 6. Lifecycle checks

| Check | How | Expected |
|---|---|---|
| Restart survival | `docker compose restart collector emulator` | logs `resuming adopted device from saved state`, connected again within ~10 s, no re-adoption |
| Switch/collector outage | `docker compose stop collector` | after 90 s the emulator logs `pausing informs`, and UniFi shows the switch *disconnected* a minute or two later |
| Recovery | `docker compose start collector` | `informing resumed`, connected within ~15 s |
| Web UI login on the GS116Ev2 | log into its web UI | the collector logs `session displaced ... yielding for 300s`, and UniFi shows the switch offline while you work in the NETGEAR UI |
| Forget in UniFi | Forget the device in the UI | emulator logs `setdefault`, device returns to pending |
| Clean removal | `docker compose down -v` | containers and adoption state removed; the controller is disposable |

## 7. Hardware-free variant

```sh
SWITCHES_CONFIG=./config/switches.mock.yaml docker compose up -d --build
```

This adds two simulated switches. Port 2 of the mock GS308Ev4 flaps every
60 s, the mock GS116Ev2 fails 5 % of polls, and a declared downlink (mock
GS308Ev4 on port 2 of the mock GS116Ev2) appears in the controller's
`downlink_table`.

## 8. Port configuration from UniFi (opt-in)

Enable writes in `config/switches.yaml`, first with `dry_run: true`. Once
live, UniFi manages **every port** of the switch, so test on a switch whose
configuration you can afford to change.

```yaml
      write: {enabled: true, dry_run: true}
```

```sh
docker compose up -d collector
python3 scripts/test_controller.py network lab-3901 3901
python3 scripts/test_controller.py port 28:94:01:8f:ff:71 4 native=3901 tagged=none name=lab-access
python3 scripts/test_controller.py port 38:94:ed:0c:d4:9b 4 native=1 tagged=3901 speed=100 duplex=full name=lab-trunk
python3 scripts/test_controller.py port 38:94:ed:0c:d4:9b 4 state=disabled
```

Check `config_sync.last_plan` in the collector status. With the test
controller's defaults it should only contain the changes you made in UniFi
(new networks are tagged on every port: UniFi's "Allow All" default). Then
set `dry_run: false` and restart the collector. While a change
is applied, UniFi shows the device as *provisioning*; afterwards it is
connected with `cfgversion == known_cfgversion`. Verify in the NETGEAR web UI
(VLAN → 802.1Q → Advanced). To undo, use `port-reset MAC 4` and
`network-delete 3901`: the integration restores the defaults and deletes the
VLAN.

## 9. Clean up

```sh
docker compose down -v      # removes containers, the emulator state and the test controller
```

## Troubleshooting

- `controller rejected inform http_status=503`: the controller is still booting.
- `telemetry stale: AuthenticationError`: wrong password in `.env`. The
  collector retries every 5 minutes to avoid locking the switch.
- `IdentityMismatchError`: a different model answers at the configured IP.
- `physical switch identity changed`: the serial behind a virtual device
  changed. If that is intended, run `docker compose exec emulator ngu-emulator
  reset <id>` and restart the emulator.
- Library debug logs: `LIBRARY_DEBUG=1` in `.env`. These **contain session
  cookies**; do not share them.
