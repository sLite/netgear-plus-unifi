# Agent guide: netgear-plus-unifi

NETGEAR Plus switches presented to UniFi Network as adopted UniFi switches,
with live port data and (opt-in) UniFi port configuration applied to the
hardware. One Go program, `cmd/netgear-unifi`, one container. Background:
[README.md](README.md), [docs/REPORT.md](docs/REPORT.md),
[docs/CONFIG-WRITE.md](docs/CONFIG-WRITE.md).

## Layout

| Path | Role |
|---|---|
| `cmd/netgear-unifi` | Wiring: config → per switch a poller + a virtual UniFi device; status endpoint; `ids`/`reset` commands |
| `cmd/unifi-testctl` | Dev tool for the disposable test controller (refuses other controllers) |
| `internal/netgear` | NETGEAR web-UI client: session/cookies, login hashing, model detection, status parsing, write backends. One file per model |
| `internal/provision` | Vendor-independent config model, UniFi `system_cfg` parser, planner, `VerifiedWriter` (guards, read-back, side-effect restore), `Sync` |
| `internal/poller` | Per-switch poll loop (backoff, session yield, rates, sync ticks); `poller.Source` is the seam for any switch implementation |
| `internal/device` | Virtual UniFi switch on unifi-emu's `inform` package: adoption state, payload overlay, cfgversion hold |
| `internal/telemetry` | Readings, snapshots, rate calculation |
| `internal/mock` | Simulated switches (dev without hardware) and an in-memory `provision.Backend` used by tests |
| `internal/config` | `config/switches.yaml` (strict: unknown keys are errors) |
| `internal/netgear/testdata/<model>/` | Pages recorded from real switches, anonymised |
| `docker-compose.yml`, `docker-compose.test.yml`, `docker-compose.coolify.yml` | Production, test-controller overlay, Coolify ([docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)) |

## Checks

```sh
gofmt -l . ; go vet ./... && go test -race -cover ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
```

Live checks against hardware and a test controller: [docs/TESTING.md](docs/TESTING.md).

## Rules

- **Physical switches are someone's network.** Reading is fine. Any write to
  a switch (including through `go run`, probes or the running service with
  writes enabled) needs the user's explicit OK for that switch and those
  ports. Never touch VLAN mode, management VLAN, IP settings, password,
  firmware, reboot or reset. Revert test changes and verify by reading back.
- **Never point anything at a production UniFi controller** (the user's
  Dream Router) without explicit approval. Use the test controller from
  `docker-compose.test.yml`.
- The GS116Ev2 allows one web session: reading it directly (probe, curl)
  displaces the running service, which then yields for 5 minutes. Stop the
  service first (`docker compose stop netgear-unifi`) and start it after.
- Never log or commit passwords, session cookies, auth keys or real
  serials/MACs in test data. Passwords come from env (`.env`, gitignored).
- Keep README, docs and CHANGELOG (`[Unreleased]`) up to date with any
  change.

## Adding a NETGEAR model

The supported models are listed in `internal/netgear/switch.go` (`models`).
Everything else (config validation, error messages, speed caps sent to
UniFi) derives from that list.

### 1. Record the pages (read-only)

With the user's OK to access the switch:

1. Find the login page and its form: `login.cgi` (GS30x family: hidden
   `rand` input, model in `<title>`) or `login.htm` (JGS family:
   `sysGeneInfor = 'MODEL?...'`). An unauthenticated GET is harmless.
2. Log in like the web UI does and save every page the implementation will
   read: system info (firmware, serial, MAC, name), port status (link,
   speed, mode, name), statistics (rx/tx bytes, CRC errors), and for writes
   the VLAN, PVID, port-settings and name pages. `.local/` (gitignored) is
   the place for throwaway probe programs; reuse `netgear.NewSwitch` where
   the family matches.
3. Anonymise before saving under `internal/netgear/testdata/<model>/`:
   serial → `SERIAL<model>TEST`, MAC → vendor OUI + `00:00:0N`, session
   hashes/`secureRand` → fixed values, switch name → empty, login `rand` →
   `123456789`. Grep the result for the real values.

Check the web UI's JavaScript for anything the HTML does not show (many
pages load fragments or post via AJAX). NETGEAR packs it as
`eval((function(x){...})("..."))`: decode it offline with a reimplementation
of the decoder; do not execute downloaded code.

### 2. Implement the model

Create `internal/netgear/<model>.go` with a type implementing `model`
(`switch.go`):

| Method | Notes |
|---|---|
| `name`, `ports`, `verifiedFirmware` | Exact model name as the switch reports it; firmware the pages were recorded from |
| `identify(loginPage)` | Must not match any other supported model |
| `reloginOnExpiry` | `true` only if the switch allows concurrent sessions; `false` for single-session models |
| `login`, `logout` | Hashes: `mergeHash` (GS30x: MD5 of password and `rand` interleaved), `hexHMACMD5` (JGS). The session cookie is stored verbatim (`session.takeCookie`). Logout must be the web UI's real logout; verify that the old cookie is rejected afterwards |
| `read` | Port readings in port order plus metadata (`withMetadata` asks for it; models whose status page carries it can always return it). `nil` pointer = not reported, never a guessed zero. Byte counters must be full 64-bit values |
| `caps` | Speed settings the switch's UI accepts (`provision.Speed`) and its VLAN limit. UniFi gets these as `speed_caps` |
| `backend` | `provision.Backend` for writes, or `nil` for a read-only model (its sync then reports `unsupported`; say so in the docs) |

Reuse the shared pieces: `session.get/post` (ordered form fields; expired
and displaced sessions handled), `parseHTML` and the matchers in `html.go`,
`parseLinkSpeed`, `parseDuplex`, `hardwareMAC`, `jgsVar`/`jgsEntries` for
JGS-style JavaScript records. If the model is a sibling of an existing one
(same page family), share the parsers instead of copying them.

Backend requirements (the `VerifiedWriter` relies on them):

- `ReadConfig` returns VLAN memberships, PVIDs, per-port speed and name,
  switch name and management VLAN, and refreshes any form tokens the writes
  need (session hash, `secureRand`, `vlanNum`, ...).
- `WriteMembers` writes the **complete** membership of one VLAN.
- Never implement VLAN mode changes, management VLAN, IP or password
  changes, reboot, reset or firmware. If a write form also carries such
  fields (the GS116Ev2 name form carries the IP settings), echo the current
  values and verify them afterwards, as `jgsWriter.SetSwitchName` does.

Then add the model to `models` in `switch.go`, and to `mock.Models` if the
simulator should offer it.

### 3. Test

Extend the fake switch in `internal/netgear/fake_test.go` (`pages()`,
login handling) and add tests like the existing per-model ones:

- a reading test against the recorded pages (identity, link/speed per port,
  counters including values above 2^32);
- login rejection and session-limit errors;
- session loss (expired or displaced, matching `reloginOnExpiry`);
- `TestPollingNeverWrites` covers the model once it is in the fake switch;
- write encoding: every request field and its order, compared with what the
  web UI sends.

### 4. Choose the UniFi model

Pick a UniFi switch with the same number of ports
(`unifi-emu` model registry, `emu.Profile`). PoE is always reported absent.
Document the choice in `docs/REPORT.md` §5 and in
`config/switches.example.yaml`.

### 5. Verify on hardware, then document

With the user's OK and on ports they name: adopt on the test controller,
compare the port table with the switch's own UI, then (if writes are
supported) run the write tests from `docs/TESTING.md` §8 and revert. Record
the results and the protocol in `docs/CONFIG-WRITE.md` (wire protocol
table), `docs/REPORT.md` (verification table) and the CHANGELOG.

## Adding another vendor

Implement `poller.Source` (`Read`, `Writer`, `Close`) in a new package, add
a `source.adapter` value in `internal/config` and the constructor in
`cmd/netgear-unifi/main.go` (`buildSource`). Readings use
`internal/telemetry`; writes go through `provision.NewVerifiedWriter` with a
vendor `provision.Backend`, so the planner, guards and read-back
verification stay shared.
