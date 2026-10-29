# Deploying alongside the Dream Router 7

> **Requires explicit approval.** Nothing in this repository has been run
> against the production DR7. Adoption writes device records into the
> production UniFi database. Do the steps below only after deciding to
> accept that, and only after the test-controller procedure in
> [TESTING.md](TESTING.md) works on your hardware.

## What changes on the DR7

- Two new switch devices (`28:94:01:8f:ff:71` USL8LP, `38:94:ed:0c:d4:9b`
  USL16LP for the ids `gs308ev4` and `gs116ev2`, with `unifi.mac: physical`) are
  adopted into the site. These are the NETGEARs' own MACs, which the DR7 so far
  lists as wired clients; once adopted, UniFi should treat them as the switches
  and may draw them in the topology (not verified yet, see the report).
- The controller will offer them UniFi firmware upgrades. These are emulated:
  the reported version changes, nothing is flashed, and the NETGEAR is never
  touched. Exclude them from automatic updates anyway.
- With `source.write` disabled (the default), port settings you change in
  UniFi are **not applied** to the NETGEARs; leave them at their defaults.
  With writes enabled, UniFi manages every port of the switch.
  Important: UniFi's default for every port is "Allow All", so every network
  (VLAN) on the DR7 site would be created on the NETGEAR and tagged on all
  ports, and VLANs configured only on the NETGEAR would be deleted. Set the
  ports to *Block All* or *Custom* in UniFi where needed, and start with
  `dry_run: true` to review the plan
  ([CONFIG-WRITE.md](CONFIG-WRITE.md)).
- Nothing is written to the DR7 database by this project directly. The
  devices enter only through the normal inform/adoption path.

## Host requirements

- Any always-on Docker host on the LAN (x86-64 or arm64), with HTTP access
  to the switches (`192.168.1.230`, `192.168.1.247`) and to the DR7 inform
  port `tcp/8080`.
- The DR7 must accept informs from that host. This is the default on the LAN;
  check firewall rules if the host is in another VLAN.
- No inbound ports are needed. The emulator status endpoint is bound to
  `127.0.0.1:8098`.

## Steps

1. Note the DR7 LAN IP (e.g. `192.168.1.1`). The inform URL must use the IP,
   not a hostname: `http://192.168.1.1:8080/inform`.
2. Prepare the config:
   ```sh
   cp .env.example .env && chmod 600 .env       # NETGEAR passwords
   cp config/switches.example.yaml config/switches.yaml
   # set emulator.inform_url: "http://192.168.1.1:8080/inform"
   ```
3. If you tested before, start from clean adoption state. The emulator also
   discards stale state automatically when the inform URL changes:
   ```sh
   docker compose down -v
   ```
4. Start **without** the test overlay:
   ```sh
   unset COMPOSE_FILE
   docker compose up -d --build
   docker compose logs -f emulator    # wait for "pending adoption"
   ```
5. In UniFi Network on the DR7: UniFi Devices → the two switches show
   *Pending Adoption*. Click **Adopt** on each. Adopt them one after the
   other; controllers reject bursts.
6. In each device's settings, disable automatic firmware updates for these
   devices (or globally) and leave port profiles at default.
7. Optional topology: if you know the cabling, declare it in
   `config/switches.yaml` (`unifi.downlinks` between the two NETGEARs). An
   uplink to the DR7 itself cannot be drawn (see the report).

## Removal

1. UniFi Network → each emulated device → Settings → **Forget/Remove**.
2. `docker compose down -v` (removes containers and adoption state including
   the auth keys).

The NETGEAR switches need no cleanup: nothing on them was changed. If you
logged into a switch's web UI during operation, the collector simply logged in
again afterwards.

## Updating

`git pull && docker compose up -d --build`. Adoption state lives in the
`emulator-state` volume and survives image rebuilds.

## Operating notes

- The GS116Ev2 allows a single web session. While you use its web UI, the
  collector backs off (`session_yield_seconds`, default 300 s) and UniFi
  shows the switch offline. The GS308Ev4 accepts concurrent sessions.
- If the NETGEAR is unreachable for more than 90 s, the virtual switch stops
  informing and UniFi marks it disconnected. It recovers by itself.
- Not tested: controller backup/restore and controller migration. If
  devices fall back to pending afterwards, run `ngu-emulator reset <id>` and
  adopt them again.
