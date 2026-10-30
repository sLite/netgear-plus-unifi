# Deploying alongside the Dream Router 7

Running since 2026-10-30 against the DR7 (`192.168.1.1`) with the GS116Ev2
at `192.168.1.5` and the GS308Ev4 at `192.168.1.6`. Before deploying a new
setup, run the test-controller procedure in [TESTING.md](TESTING.md) on your
hardware: adoption writes device records into the production UniFi database.

## What changes on the DR7

- Two switch devices (`28:94:01:8f:ff:71` USL8LP and `38:94:ed:0c:d4:9b`
  USL16LP for the ids `gs308ev4` and `gs116ev2`, with `unifi.mac: physical`)
  are adopted into the site. These are the NETGEARs' own MACs.
- The controller will offer them UniFi firmware upgrades. These are emulated:
  the reported version changes, nothing is flashed, and the NETGEAR is never
  touched. Exclude them from automatic updates anyway.
- With `source.write` disabled (the default), port settings you change in
  UniFi are **not applied** to the NETGEARs. With writes enabled, UniFi
  manages every port of the switch. UniFi's default for every port is
  "Allow All", so every network (VLAN) on the DR7 site is created on the
  NETGEAR and tagged on all ports, and VLANs configured only on the NETGEAR
  are deleted. Set ports to *Block All* or *Custom* in UniFi where needed,
  and start with `dry_run: true` to review the plan
  ([CONFIG-WRITE.md](CONFIG-WRITE.md)).
- Nothing is written to the DR7 database by this project directly. The
  devices enter only through the normal inform/adoption path.

## Host requirements

- Any always-on Docker host on the LAN (x86-64 or arm64), with HTTP access
  to the switches and to the DR7 inform port `tcp/8080`.
- The DR7 must accept informs from that host. This is the default on the LAN;
  check firewall rules if the host is in another VLAN.
- No inbound ports are needed. The status endpoint is bound to
  `127.0.0.1:8098`.
- **Run exactly one instance.** Two instances would inform the DR7 as the same
  devices, and the GS116Ev2 (one web session) would log one of them out on
  every poll.

## First deployment

1. Note the DR7 LAN IP. The inform URL must use the IP, not a hostname:
   `http://192.168.1.1:8080/inform`.
2. Prepare the config:
   ```sh
   cp .env.example .env && chmod 600 .env       # NETGEAR passwords
   cp config/switches.example.yaml config/switches.yaml
   # set controller.inform_url and each source.host;
   # keep write.dry_run: true for the first start
   ```
3. If you tested before, start from clean adoption state. The service also
   discards stale state automatically when the inform URL changes:
   ```sh
   docker compose down -v
   ```
4. Start (`docker-compose.yml` only; the test overlay is used only when you
   name it with `-f` or `COMPOSE_FILE`):
   ```sh
   docker compose up -d --build
   docker compose logs -f netgear-unifi    # wait for "pending adoption"
   ```
5. In UniFi Network on the DR7: UniFi Devices → the two switches show
   *Pending Adoption*. Click **Adopt** on each, one after the other;
   controllers reject bursts.
6. In each device's settings, disable automatic firmware updates for these
   devices (or globally).
7. With writes enabled: check `config_sync.last_plan` in
   `curl -s 127.0.0.1:8098/status`, adjust the ports in UniFi until the plan
   is what you want, then set `dry_run: false` and restart.
8. Optional topology: declare each switch's uplink (`unifi.uplink`, with the
   gateway's device MAC as shown in UniFi) and links between the NETGEARs
   (`unifi.downlinks`). UniFi then marks the uplink ports and shows the
   gateway on them; it does not draw topology lines
   ([REPORT.md](REPORT.md) §6).

## Running the service

All commands run in the repository directory.

| Task | Command |
|---|---|
| Start (and build if needed) | `docker compose up -d --build` |
| Restart (e.g. after editing `config/switches.yaml` or `.env`) | `docker compose restart netgear-unifi` |
| Stop (logs out of both switches) | `docker compose stop` |
| Start after a stop | `docker compose start` |
| Logs | `docker compose logs -f netgear-unifi` |
| Status: devices, switch telemetry, config sync | `curl -s 127.0.0.1:8098/status` |
| Running state and health | `docker compose ps` |
| Debug logging | `LOG_LEVEL=DEBUG` in `.env`, then `docker compose up -d` (recreates the container) |
| UniFi device MACs | `docker compose exec netgear-unifi netgear-unifi ids` |

- The configuration is read at start: after changing `config/switches.yaml`
  a **restart** is enough. Changes to `.env` take effect with
  `docker compose up -d` (a plain restart keeps the old environment).
- After a code update: `git pull && docker compose up -d --build`. Adoption
  state lives in the `state` volume and survives rebuilds and restarts.
- The container restarts automatically after a crash or host reboot
  (`restart: unless-stopped`), unless you stopped it.

## Moving to another host

Everything the adoption depends on is the config, the passwords and the
adoption state (`state` volume, contains the controller-issued auth keys).
Copy all three and the switches stay adopted; the DR7 does not care which
host informs it. Without the state, the devices must be forgotten and adopted
again, which also deletes their port settings in UniFi.

1. Old host: stop the service and export the state. The image has no shell,
   so a helper container copies the volume:
   ```sh
   docker compose stop
   docker run --rm -v netgear-plus-unifi_state:/data -v "$PWD":/backup alpine \
     tar czf /backup/state.tgz --numeric-owner -C /data .
   ```
2. Copy the repository (or clone it), `config/switches.yaml`, `.env` and
   `state.tgz` to the new host.
3. New host: restore the volume, then start:
   ```sh
   docker volume create netgear-plus-unifi_state
   docker run --rm -v netgear-plus-unifi_state:/data -v "$PWD":/backup alpine \
     tar xzf /backup/state.tgz --numeric-owner -C /data
   docker compose up -d --build
   ```
   The volume name is the same on every host: the compose file fixes the
   project name.
4. Delete `state.tgz` (it contains the auth keys). On the old host:
   `docker compose down -v`.

## Migrating to Coolify

`docker-compose.coolify.yml` is the same service for Coolify's Docker Compose
build pack: passwords are Coolify environment variables, and configuration
and state are bind mounts under `/srv/netgear-unifi` on the Coolify server.

Requirements: the Coolify server is on the LAN with the same access as above,
and Coolify can read this repository (a Git remote it is connected to, e.g. a
private GitHub/Gitea repository with a deploy key).

1. **Prepare the Coolify server** (as root), before the first deployment:
   ```sh
   mkdir -p /srv/netgear-unifi/state
   # copy config/switches.yaml here (contains no secrets)
   cp switches.yaml /srv/netgear-unifi/switches.yaml
   chmod 644 /srv/netgear-unifi/switches.yaml
   ```
2. **Carry over the adoption state.** On the old host, stop the service and
   export the state (step 1 of *Moving to another host*), copy `state.tgz`
   to the Coolify server, then:
   ```sh
   tar xzf state.tgz --numeric-owner -C /srv/netgear-unifi/state
   chown -R 65532:65532 /srv/netgear-unifi/state   # the container's nonroot user
   chmod 700 /srv/netgear-unifi/state
   rm state.tgz
   ```
   For a fresh installation skip the copy but keep the `chown`.
3. **Create the resource** in Coolify: New Resource → your repository →
   Build Pack **Docker Compose**, Docker Compose Location
   `/docker-compose.coolify.yml`. No domain is needed (the service has no
   web UI; the status endpoint stays on `127.0.0.1:8098` of the server).
4. **Environment Variables** (Coolify creates them from the compose file):
   set `NETGEAR_GS308EV4_PASSWORD` and `NETGEAR_GS116EV2_PASSWORD`;
   optionally `LOG_LEVEL` and `UNIFI_INFORM_URL`. If you add a switch with
   another `password_env`, add a matching line to the compose file.
5. **Make sure the old instance is stopped**, then deploy in Coolify. In the
   deployment logs, expect `resuming adopted device from saved state` for
   both switches; on the server, `curl -s 127.0.0.1:8098/status` shows both
   connected.
6. Remove the old installation: `docker compose down -v` on the old host.

Operating on Coolify: Restart/Stop/Redeploy in the resource's UI replace the
`docker compose` commands above. Edit `/srv/netgear-unifi/switches.yaml` on
the server, then restart the resource. Commands inside the container, e.g.
`netgear-unifi reset <id>`, go through Coolify's terminal or
`docker exec <container> netgear-unifi ...` on the server.

Not tested on a real Coolify instance. The compose file follows Coolify's
documented Compose handling (environment variables from `${VAR}`, absolute
bind mounts shown under Persistent Storage); check the first deployment's
logs.

## Removal

1. UniFi Network → each emulated device → Settings → **Forget/Remove**.
2. `docker compose down -v` (removes the container and adoption state
   including the auth keys). On Coolify: delete the resource and
   `/srv/netgear-unifi`.

With writes disabled (the default) the NETGEAR switches need no cleanup:
nothing on them was changed. With writes enabled they keep the last
configuration UniFi applied (VLANs, PVIDs, port settings, name); forgetting
the device does not revert it. Reset those settings in UniFi before
forgetting the device, or change them in the NETGEAR web UI afterwards.

Coming from the earlier two-service version (Python `collector` + Go
`emulator`): move `emulator.inform_url` to `controller.inform_url` and the
other settings as described in the [changelog](../CHANGELOG.md), forget the
devices in UniFi, run `docker compose down -v --remove-orphans`, then start
and adopt again. Adoption state is not migrated.

## Operating notes

- The GS116Ev2 allows a single web session. While you use its web UI, the
  service backs off (`session_yield_seconds`, default 300 s) and UniFi
  shows the switch offline. The GS308Ev4 accepts concurrent sessions.
  `docker compose restart netgear-unifi` after logging out of the web UI
  ends the wait early.
- If a NETGEAR is unreachable for more than 90 s, its virtual switch stops
  informing and UniFi marks it disconnected. It recovers by itself.
- Not tested: controller backup/restore and controller migration. If
  devices fall back to pending afterwards, run `docker compose exec
  netgear-unifi netgear-unifi reset <id>` and adopt them again.
- Replacing the DR7 without a backup: the new controller does not know the
  devices. Before re-adopting, set `dry_run: true` (or disable writes):
  a fresh controller pushes its defaults ("Allow All" with only the networks
  that exist on it), which would delete VLANs not yet recreated. Recreate
  networks and port settings in UniFi, check `config_sync.last_plan`, then
  go live again.
