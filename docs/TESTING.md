# Testing against a disposable UniFi controller

Everything here runs on one Docker host on the same LAN as the switches.
The test controller is a throwaway container and never touches the Dream
Router. Unless you enable `source.write`, the only traffic to the switches
is read-only HTTP polling.

## 1. Automated tests (no hardware, no controller)

```sh
go vet ./... && go test -race -cover ./...
```

The NETGEAR tests run against a fake switch HTTP server that serves pages
recorded from both real switches (`internal/netgear/testdata`, serials,
MACs and session hashes anonymised) and emulates login, session cookies,
expired and displaced sessions.

## 2. Configure

```sh
cp .env.example .env            # NETGEAR_GS308EV4_PASSWORD=..., NETGEAR_GS116EV2_PASSWORD=...
chmod 600 .env
cp config/switches.example.yaml config/switches.yaml
```

The example already points at `192.168.1.247` (GS308Ev4) and `192.168.1.230`
(GS116Ev2). Leave `controller.inform_url` alone: the test overlay overrides it.

## 3. Start the service and the test controller

```sh
export COMPOSE_FILE=docker-compose.yml:docker-compose.test.yml
docker compose up -d --build
docker compose logs -f netgear-unifi   # Ctrl-C when "pending adoption" appears
```

The controller needs 1–3 minutes to boot (the service logs HTTP 503 until
it is ready). Expected log lines:

```
... logged in switch=gs308ev4 host=192.168.1.247 model=GS308Ev4
... bound to physical switch switch=gs308ev4 mac=28:94:01:8f:ff:71 physical_model=GS308Ev4 firmware=V1.0.1.9
... pending adoption: waiting for Adopt in UniFi Network switch=gs308ev4 mac=28:94:01:8f:ff:71
```

## 4. Adopt

Either open <https://127.0.0.1:18443> (accept the self-signed certificate;
login `admin` / `unifi-containers-seeded`) → UniFi Devices → *Adopt* on both
switches, or use the test-controller helper:

```sh
go run ./cmd/unifi-testctl devices
go run ./cmd/unifi-testctl adopt 28:94:01:8f:ff:71 38:94:ed:0c:d4:9b
go run ./cmd/unifi-testctl wait  28:94:01:8f:ff:71 38:94:ed:0c:d4:9b
```

The helper refuses any controller outside loopback and Docker networks, so
it cannot be pointed at the DR7 by accident.

## 5. Verify the success criterion

```sh
go run ./cmd/unifi-testctl devices
go run ./cmd/unifi-testctl ports 38:94:ed:0c:d4:9b
curl -s 127.0.0.1:8098/status
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
| Restart survival | `docker compose restart netgear-unifi` | logs `resuming adopted device from saved state`, connected again within ~10 s, no re-adoption; both switches logged out on stop |
| Switch outage | unplug a switch or block its IP | after 90 s the service logs `pausing informs`, and UniFi shows the switch *disconnected* a minute or two later |
| Recovery | reconnect | `informing resumed`, connected within ~15 s |
| Web UI login on the GS116Ev2 | log into its web UI | the service logs `session displaced ... yielding for=5m0s`, and UniFi shows the switch offline while you work in the NETGEAR UI |
| Forget in UniFi | Forget the device in the UI | the service logs `setdefault`, the device returns to pending |
| Clean removal | `docker compose down -v` | containers and adoption state removed; the controller is disposable |

## 7. Hardware-free variant

```sh
SWITCHES_CONFIG=./config/switches.mock.yaml docker compose up -d --build
```

This uses two simulated switches. Port 2 of the mock GS308Ev4 flaps every
60 s, the mock GS116Ev2 fails 5 % of polls, and a declared downlink (mock
GS308Ev4 on port 2 of the mock GS116Ev2) appears in the controller's
`downlink_table`. Port configuration writes go to the simulated switch.

## 8. Port configuration from UniFi (opt-in)

Enable writes in `config/switches.yaml`, first with `dry_run: true`. Once
live, UniFi manages **every port** of the switch, so test on a switch whose
configuration you can afford to change.

```yaml
      write: {enabled: true, dry_run: true}
```

```sh
docker compose up -d
go run ./cmd/unifi-testctl network lab-3901 3901
go run ./cmd/unifi-testctl port 28:94:01:8f:ff:71 4 native=3901 tagged=none name=lab-access
go run ./cmd/unifi-testctl port 38:94:ed:0c:d4:9b 4 native=1 tagged=3901 speed=100 duplex=full name=lab-trunk
go run ./cmd/unifi-testctl port 38:94:ed:0c:d4:9b 5 state=disabled
go run ./cmd/unifi-testctl name 38:94:ed:0c:d4:9b "Switch Office"
```

Check `config_sync.last_plan` in `curl -s 127.0.0.1:8098/status`. With the
test controller's defaults it should only contain the changes you made in
UniFi (new networks are tagged on every port: UniFi's "Allow All" default).
Then set `dry_run: false` and restart the service. While a change is
applied, UniFi shows the device as *provisioning*; afterwards it is
connected. Verify in the NETGEAR web UI (VLAN → 802.1Q → Advanced). To
undo, use `port-reset MAC 4` and `network-delete 3901`: the integration
restores the defaults and deletes the VLAN.

Drift check: change a managed setting in the NETGEAR web UI. The service
restores UniFi's setting within 5 minutes, or on its next start.

## 9. Clean up

```sh
docker compose down -v      # removes the container, its adoption state and the test controller
```

## Troubleshooting

- `controller rejected inform http_status=503`: the controller is still booting.
- `telemetry stale: switch rejected the login`: wrong password in `.env`.
  The service retries every 5 minutes to avoid locking the switch.
- `unexpected switch model`: a different model answers at the configured IP.
- `physical switch identity changed`: the serial behind a virtual device
  changed. If that is intended, run `docker compose exec netgear-unifi
  netgear-unifi reset <id>` and restart the service.
- Request-level logs: `LOG_LEVEL=DEBUG` logs every switch request (method,
  path, status, duration; never cookies or passwords) and the redacted
  configuration UniFi sends.
