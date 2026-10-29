# Technical report: NETGEAR Plus switches in UniFi Network

Date: 2026-10-29. Hardware: NETGEAR GS308Ev4 (V1.0.1.9) at 192.168.1.247,
GS116Ev2 (2.6.0.50) at 192.168.1.230. Controller: disposable UniFi Network
**10.6.109** (`ghcr.io/jamesbraid/unifi-network:10.6.109-seeded`). The
production Dream Router 7 was **not** touched.

## 1. Result

Both physical switches appear in UniFi Network as separate, adopted,
**connected** UniFi switches. Their port tables come from the hardware: link
state, negotiated speed, duplex (where the switch reports it), byte counters
and rates. The devices stay connected through the normal inform lifecycle,
survive restarts without re-adoption, go offline in UniFi when their NETGEAR
is unreachable, and recover automatically.

The integration is one self-contained Go program (`netgear-unifi`, one
container). It started as a Python collector (wrapping py-netgear-plus) plus
a Go emulator; both were merged into Go, and every result below was
re-verified with the Go version on both switches unless noted.

Port management (opt-in per switch): VLANs, native VLAN, tagged VLANs, port
disable, forced speed and port names set in UniFi's port editor are applied
to the switch, which UniFi then manages like a native switch. Live writes were
verified on ports 4 and 5 of both switches (the ports cleared for testing);
the whole-switch plan was verified in dry run. The device name is applied as
the switch name. UniFi shows the device as
*provisioning* until the NETGEAR matches. See [CONFIG-WRITE.md](CONFIG-WRITE.md).

Example: controller-side port table of the GS116Ev2, read back via the
controller API.

```
port  1 up=True  speed=10    rx=0           tx=62280659    tx/s=249.6
port  2 up=True  speed=1000  rx=3117597626  tx=23556576321 rx/s=7918.4 tx/s=14763.8
port  3 up=False speed=0 ...
port  8 up=True  speed=1000  rx=25313585439 tx=3206072242  rx/s=12901.5 tx/s=7765.7
port 12 up=True  speed=100   ...
```

This matches the NETGEAR's own pages: ports 1 (10M), 2, 7, 8, 15 (1G) and
12 (100M) up.

## 2. What was verified, and against what

| Capability | Real controller (10.6.109) | Real NETGEAR hardware | Automated tests |
|---|---|---|---|
| Discovery-free adoption (pending → adopt → connected, CBC → AES-GCM) | ✅ host run and Docker Compose | – | ✅ fake controller |
| Two independent virtual switches (8 + 16 ports) | ✅ | ✅ both models | ✅ |
| Link state, negotiated speed, byte counters, rates in `port_table` | ✅ via `stat/device` API | ✅ | ✅ recorded real pages |
| Link change propagates to controller | ✅ (mock flapping port, Python version) | ⚠️ not tested: no cable was unplugged on your network | ✅ |
| Restart survival (process restart, container recreate) | ✅ | ✅ both switches logged out on stop | ✅ |
| Outage → offline in UniFi → automatic recovery | ✅ (Python version, collector stopped: informs paused after 90 s, controller showed *disconnected* ~2 min later, reconnected ~15 s after restart) | ✅ (Python version) | ✅ |
| Session displaced by web-UI login (GS116Ev2) → yield → recover | ✅ shown disconnected during the yield | ✅ displaced, informs paused after 90 s, recovered after the 5-minute yield | ✅ |
| Counter reset handling | – | ⚠️ not tested: counters were never cleared on the switches | ✅ |
| 64-bit counters (> 4 GiB) | – | ✅ GS116Ev2 (25 GB counters); GS308Ev4 only from the library's recorded GS108Ev4 pages | ✅ |
| Declared topology between the two switches (`downlink_table`) | ✅ (mock pair, Python version) | – | ✅ |
| Controller provisioning captured | ✅ VLANs, PVID, disable, speed | – | ✅ |
| **Port configuration from UniFi applied to hardware** (opt-in; live-tested on ports 4/5, whole switch in dry run) | ✅ VLAN create/delete, access port, trunk with custom tagged VLANs (Python version), native VLAN, disable, forced speed, name, switch name; provisioning → connected | ✅ both models, verified by read-back; drift corrected | ✅ |
| Re-adopting a switch configured outside UniFi | ✅ UniFi imports nothing (see §6) | ✅ GS308Ev4 (Python version) | – |
| Forget in UniFi (`setdefault`) → pending | – | – | ✅ fake controller only |
| UI rendering | ⚠️ verified through the controller API that feeds the UI, not by visual inspection | | |

Nothing was tested against the DR7. UniFi OS consoles run the same Network
application, but its version may differ from 10.6.109.

## 3. Architecture and reuse decisions

| Upstream | Decision | Why |
|---|---|---|
| **unifi-emu** (MIT) | Go module dependency, `inform` package **unmodified** | Correct TNBU/AES-CBC/AES-GCM/zlib implementation, adoption state machine handling both key-delivery channels, and a model registry with 400+ profiles. Its device loop has no hook for live data and no persistence, so we drive `inform.Session` directly: `BuildPayload` → overlay live tables → `Packet.Encode(GCM)` → `Apply`. Persisted state is restored by replaying set-adopt/setparam/setstate through the public `Apply` API. A unit test pins that behaviour. |
| **unifi-stubd** (AGPL-3.0) | Reference only | Richer payload (mac_table, lldp_table, uplink, if_table) and good protocol notes, but copying code would make this project AGPL, and it targets Linux bridge observation. Field names were cross-checked against it. |
| **py-netgear-plus** (Apache-2.0) | Used by the first (Python) version, pinned to `552b041`; now replaced | Its model detection, login hashing (MD5 merge / HMAC-MD5) and status-page layouts for the two models were reimplemented in Go (`internal/netgear`), with its fixes and gaps (§4) built in. Only two models are needed, which made a dependency on a Python runtime and on library internals hard to justify. |

One Go program, no other infrastructure. Per NETGEAR it runs a poller
(reads the switch, derives rates, applies UniFi configuration between reads
on the same session) and a virtual UniFi device (informs the controller).
They share state in memory; there is no internal API. Dependencies:
unifi-emu, `golang.org/x/net/html` and `gopkg.in/yaml.v3`. The physical
identity (`source:`) and virtual identity (`unifi:`) are separate sections
of one config file. The virtual device's MAC is configurable: the NETGEAR's
own MAC (`physical`, see §6 for why) or a derived locally administered one
(`auto`).

## 4. Findings about py-netgear-plus (worth reporting upstream)

All of these are handled in the Go implementation.

1. **GS108Ev4/GS308Ev4 byte counters wrap at 4 GiB.** The statistics page
   carries every counter as two 32-bit words (`hi, lo`). The parser reads only
   `lo` (`data[1]`, `data[3]`, `data[5]`). The library's own GS108Ev4 fixtures
   show `hi=25` constant while `lo` grows. Handled in
   `internal/netgear/gs308ev4.go::gs308Counters`.
2. **`get_switch_infos()` is lossy.** It converts counters to MB rounded to
   10 kB and applies filters. Raw bytes are kept ("real reported units":
   bytes on both models).
3. **GS116Ev2 session loss is not detected.** A displaced session gets HTTP
   200 with `parent.location.href='/login.htm'`. `_is_authenticated` checks
   only for a "redirect to login" title and `/wmi/login`, so the library
   raises a parse error instead of logging in again. This page is detected
   and the poller yields instead of logging the web-UI user out.
4. Library DEBUG logs contain session cookies and request parameters
   (including the password hash). The Go version logs method, path, status
   and duration only.
5. **The GS308Ev4 is the same web-UI generation as the GS30x series.** Its
   `vlan.cgi` page is parsed correctly by the library's GS30x VLAN parser,
   although the library gives the GS308Ev4 no VLAN templates. The GS30x
   VLAN/PVID/port write protocol works on the GS308Ev4 (verified, see
   [CONFIG-WRITE.md](CONFIG-WRITE.md)).
6. Firmware: GS308Ev4 **V1.0.1.9** (library documents 1.0.1.3) parsed
   correctly for model, firmware, serial, MAC, link, speed, duplex and
   counters. No differences found in the pages used here.
7. **GS116Ev2 logout does not work.** The library posts `logout.cgi`, which
   the GS116Ev2 answers with 404. Its web UI logs out by posting the
   `logoutBtn` form to `index.htm` (verified: the old session cookie is
   rejected afterwards).
8. **GS116Ev2 error responses stall.** The switch sends the headers of a
   404 at once but keeps the body open until a timeout; the library avoids
   this by not reading non-200 bodies, which the Go version does too.
9. **GS308Ev4 session cookie.** The `SID` value contains characters such as
   `\` that strict cookie parsers (Go's `net/http`) drop; it must be handled
   verbatim.

## 5. Model profiles

The controller derives PoE presence from each port's reported `poe_caps`.
With `poe_caps=0` it stores no PoE fields at all (verified). The port
editor's speed options come from each port's reported `speed_caps`, which the
integration sets per NETGEAR model (see below). The model code mainly drives the
device image and name. Both switches use the USW Lite line, so they look
alike in UniFi.

| NETGEAR | Chosen UniFi model | Notes |
|---|---|---|
| GS308Ev4 (8× GE) | **USL8LP**, USW Lite 8 PoE | 8× GE. Every port reports `poe_caps=0`, but the model *name* contains "PoE". Previously `US8` (UniFi Switch 8); changed to match the 16-port model line. |
| GS116Ev2 (16× GE) | **USL16LP**, USW Lite 16 PoE | The only 16-port GE profile with exactly 16 ports. Every port reports `poe_caps=0`, but the model *name* contains "PoE". |

Port speed options (`speed_caps`, bit values from the UI's `SPEED_CAPS`
table): GS308Ev4 advertises auto and 10/100/1000 full (1048618), GS116Ev2
auto, 10/100 half and full and 1000 full (1048623). Neither switch can force
1000 Mbit/s; UniFi's "1 GbE" setting is applied as auto, since 1000BASE-T
requires autonegotiation. 1000 full must stay advertised, because the UI
derives the port's displayed speed class from the highest bit.

Rejected: `US16P150`/`USL16P` (18 ports; two would be fabricated SFP ports),
`USXG` (10G/SFP+), `USMINI` (5 ports), `USM8P` (PoE on every port).
The UniFi catalogue has no 16-port GE switch without PoE. Any model can be
set per switch via `unifi.model`.

Firmware version: the virtual devices report the profile's UniFi version
(7.4.1.16850), not the NETGEAR firmware. The controller version-gates
features, and a NETGEAR version string is meaningless to it. The controller
then offers an upgrade to 7.5.15. An upgrade is emulated (version changes,
nothing is flashed).

## 6. Limitations

**H** = hardware/firmware does not expose the data; **P** = UniFi
protocol/architecture limitation of emulation; **I** = implementation work
not done yet.

| Not supported | Class | Detail |
|---|---|---|
| Clients per port, client→switch placement | H | Neither switch exposes a MAC/FDB table on its web UI, so `mac_table` stays empty. Clients keep appearing behind the DR7 port. Nothing is fabricated. |
| LLDP neighbours | H | Neither model shows LLDP neighbours. |
| Uplink to the DR7 in the topology view | P | The controller builds topology from upstream devices' FDB/LLDP. The DR7 sees the NETGEAR's *physical* MAC and client MACs, never the virtual MAC, so it cannot link them. A device-reported `uplink` object is ignored, and a reported `lldp_table` is stored but did not create an uplink on the gateway-less test controller (tested, then removed from the code). Reporting the NETGEAR's hardware MAC as a second `ethernet_table` interface does not work either: the controller replaces the device-reported `ethernet_table` with its own (tested, removed). **Now addressed with `unifi.mac: physical`:** the virtual device uses the NETGEAR's own MAC, the address the DR7 sees on its port, which is how the controller places real UniFi switches. Adoption under the NETGEAR MACs works (tested); whether the DR7 then draws the uplink is untested (the test controller has no gateway). |
| Topology between the two NETGEARs | ✅ opt-in | Works when declared (`unifi.downlinks`): the controller builds `downlink_table` from the declared `mac_table` entry (verified on the mock pair). Not auto-detected. |
| Packet counts, errors other than CRC, broadcast/multicast | H | Only rx/tx bytes and CRC errors are exposed (CRC is reported as `rx_errors`). |
| Duplex on GS116Ev2 | H | Not reported. Only ≥ 1 Gbit/s links are reported full duplex (1000BASE-T half duplex does not occur in practice); otherwise the field is omitted. |
| CPU, memory, temperature, fan | H | Not exposed. The integration removes unifi-emu's placeholder values instead of showing them. |
| Switch uptime | H | Not exposed. `uptime` is the virtual device's persisted boot time. |
| PoE | H | Not PoE switches. |
| Configuration from UniFi beyond VLAN/native VLAN/disable/speed/name | P/H | LAG, STP, storm control, 802.1X, mirroring, isolation: not mapped. Forced 1000 Mbit/s is not offered by either switch; half duplex is not offered by the GS308Ev4. See [CONFIG-WRITE.md](CONFIG-WRITE.md). |
| Locate/LED, reboot, port power-cycle, SSH, debug console, speed test | P/I | Commands are ignored or emulated. |
| Features gated on genuine capability bitmaps (mirroring, LAG, STP tuning, storm control, 802.1X, routing) | P | The virtual device reports placeholder `fw_caps`. UniFi may still offer some settings; they are recorded, not applied. |
| Discovery (L2 broadcast) | I (not needed) | Adoption works through inform alone. |
| Long-term statistics graphs | not verified | The controller receives correct counters and rates; history was not observed over hours. |

Operational caveats:

- The GS116Ev2 allows one web session. The service yields for
  `session_yield_seconds` (300 s) when displaced, and UniFi shows the switch
  offline meanwhile.
- With `ip: physical` UniFi shows the NETGEAR's IP. SSH/"open device"
  actions from UniFi will not work.
- UniFi never reads configuration from a device; it only shows status. Verified
  by re-adopting the GS308Ev4 after configuring it in its own web UI (VLANs
  100/200, PVID, port name, disabled port, static IP): UniFi showed DHCP,
  only the Default network and default port settings, ignored the reported
  port name, and did not show the disabled port anywhere. With writes on,
  UniFi's defaults would have replaced the switch's configuration, as with a
  native UniFi switch.
- IP settings (DHCP/static) changed in UniFi are acknowledged but never
  applied; UniFi then shows settings the switch does not have.

## 7. Next steps

1. Run the procedure in [TESTING.md](TESTING.md) yourself, including an
   unplugged-cable test. Then decide on DR7 deployment
   ([DEPLOYMENT.md](DEPLOYMENT.md)).
2. Report findings 1–3, 5 and 7 of §4 upstream to py-netgear-plus.
3. Optional topology improvement: read the DR7's client list through the
   controller API to find the NETGEAR physical MAC on a DR7 port. That would
   at least let the uplink port be declared automatically. This needs
   controller API credentials and is not implemented.
4. Port management: review the whole-switch dry-run plan against your real
   UniFi configuration before setting `dry_run: false`. Optional: ask
   py-netgear-plus upstream to adopt the GS308Ev4/GS116Ev2 write templates.

## 8. Feasibility of full VLAN and port management

**Implemented** for VLAN creation/deletion, tagged/untagged membership,
PVID/native VLAN, port enable/disable, port speed (within each switch's
options) and port names, on both models. The write protocols were
recovered from the switches' own web-UI JavaScript and verified on ports 4
and 5 of each switch. The GS308Ev4 uses the GS30x protocol that py-netgear-plus
implements for other models; the GS116Ev2's JGS form protocol was
reverse-engineered.

Not reachable on this hardware, or not mapped: LAG, STP tuning, storm
control, 802.1X, port isolation/mirroring, forced 1000 Mbit/s, PoE. Parity
with native UniFi switches is therefore limited to the port and VLAN basics.
