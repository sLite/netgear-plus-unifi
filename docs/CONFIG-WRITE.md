# UniFi → NETGEAR port configuration (opt-in)

Status: **implemented and verified on hardware** (GS308Ev4 V1.0.1.9,
GS116Ev2 2.6.0.50, UniFi Network 10.6.109 test controller). Read-only
remains the default. With writes enabled for a switch, UniFi manages **all**
of its ports, like a native UniFi switch.

## What can be managed from UniFi

| UniFi port setting | NETGEAR result | GS308Ev4 | GS116Ev2 |
|---|---|---|---|
| Native VLAN / network | untagged membership + PVID | ✅ | ✅ |
| Tagged VLAN management (Allow All / Block All / Custom) | tagged memberships | ✅ | ✅ |
| Networks (VLAN ids) | 802.1Q VLAN created when a port uses it; VLANs UniFi does not define, or that no port uses, are deleted (never VLAN 1 or the management VLAN) | ✅ | ✅ |
| Port disabled | speed "Disable" | ✅ | ✅ |
| Link speed: auto, 10/100 full | forced speed | ✅ | ✅ |
| Link speed: 10/100 half | forced speed | ❌ not offered by the GS308Ev4 UI | ✅ |
| Link speed: 1000 forced | | ❌ (neither switch) | ❌ |
| Port name | port name / description | ✅ | ✅ |
| Device name | switch name, from UniFi's hostname form (`resolv.host.1.name`: spaces and non-ASCII characters removed by UniFi, e.g. "Switch Büro" → `SwitchBro`), at most 20 characters | ✅ `switch_name.cgi` | ✅ via the IP-settings form; IP mode/address/mask/gateway are echoed unchanged and verified afterwards |

Not mapped: management VLAN (never touched), LAG, STP, storm control,
802.1X, mirroring, isolation, rate limits, voice VLAN, jumbo frames.

## Enabling it

```yaml
switches:
  - id: gs308ev4
    source:
      ...
      write:
        enabled: true      # default false: read-only
        dry_run: true      # log and show the plan only; set false to apply
        debounce_seconds: 10
```

Watch the plan before going live:

```sh
curl -s 127.0.0.1:8098/status   # see config_sync per switch
```

`config_sync.state` is one of `disabled`, `waiting`, `pending`, `dry_run`,
`in_sync`, `failed` or `unsupported`, with `last_plan`, `warnings`,
and `last_error`.

## How it works

```
UniFi ──setparam.system_cfg──▶ UniFi device ──switch section──▶ sync (on the poller)
            (switch.* lines,    (internal/device)  (persisted)  │ parse → desired
             secrets removed)                                    │ read switch → actual
                                                                 │ plan (all ports)
UniFi ◀──cfgversion acknowledged── UniFi device ◀──processed──── │ apply + read-back verify
```

1. UniFi sends switch configuration as `setparam.system_cfg`, a flat
   `key=value` document (see [captures/](captures/system_cfg-switch-section.txt)).
   The UniFi device keeps only `switch.*` lines and the hostname, drops
   credentials (`switch.wevent.key`), persists them with its adoption state
   and hands them to the switch's sync. After a restart the stored
   configuration is handed over again, so the switch is re-verified.
2. The sync (`internal/provision`) resolves UniFi's VLAN defaults and per-port overrides into an
   explicit membership per (VLAN, port) for every port, reads the switch, and
   diffs the two. Ordering keeps ports connected: add memberships → move PVIDs
   → remove memberships → delete VLANs that UniFi does not define or that no
   port uses → speed/admin state/name. Each VLAN change is a single write
   covering all ports.
3. Each operation is applied on the polling session (one switch session, no
   overlap with reads) and verified by reading back.
4. The UniFi device reports the new `cfgversion` to UniFi only after the
   sync has processed it. UniFi therefore shows *provisioning* (device state 5)
   while the NETGEAR is being configured, as with a native switch. The hold is
   capped at 2 minutes.

### Safety layers

| Layer | Guarantee |
|---|---|
| Config | Off by default (read-only); `dry_run: true` by default |
| Planner | VLAN 1 and the switch's management VLAN are never deleted (the GS308Ev4 reports its management VLAN; the GS116Ev2 manages on VLAN 1). Membership changes carry only the ports that change. |
| Writer | Re-reads the switch immediately before writing and merges the change into that fresh state. Refuses nonexistent ports, VLAN ids outside 1–4093, empty VLANs, and deletion of the default/management VLAN. |
| Verification | After every write, every VLAN's membership and every PVID is compared with the exact state the operation should produce. Anything the operation did not name that changed is **restored** immediately and the apply fails. |
| Not implemented at all | VLAN mode changes (`vlanMod.cgi`, GS116Ev2 `changeType=1`, which would reset all VLANs), management VLAN changes (`switch.managementvlan` is ignored), IP, password, reboot, reset, firmware |
| Failure handling | Up to 3 attempts with backoff, then `failed` (and acknowledged, so UniFi does not loop). Drift (changes made in the NETGEAR web UI) is re-checked every 5 minutes and corrected. |

As with a native UniFi switch, a UniFi port configuration that takes VLAN 1
(or the management VLAN) away from the port you manage the switch through
will cut off management access. Review the dry-run plan first.

## Wire protocols (taken from the switches' own web-UI code)

**GS308Ev4**: same CGI generation as the GS30x series supported by
py-netgear-plus.
The packed JavaScript was decoded offline.

| Operation | Request |
|---|---|
| Read | `GET vlan.cgi` (memberships, PVIDs, `hash`, `vlanNum`, voice VLAN), `GET dashboard.cgi` (port speed/name/flow/rates) |
| VLAN add / edit / delete | `POST 8021qAdvanced.cgi` `hash, ACTION=Add\|Apply\|Delete, VLAN_ID, vlanNum, VLAN_NAME, hiddenMem, voiceVLANID, voiceVlanCos`; `hiddenMem` has one digit per port (1=T, 2=U, 3=excluded). Success = HTML fragment `<div…` |
| PVID | `POST portPVID.cgi` `hash, PORT, PVID` |
| Switch name | `POST switch_name.cgi` `hash, switch_name, SET_LANG` (current UI language). Success = `SWITCH NAME SUCCESS` |
| Speed/disable/name | `POST port_status.cgi` `hash, port<N>=checked, SPEED (1 auto, 2 disable, 4 10M full, 6 100M full), FLOW_CONTROL, DESCRIPTION, IngressRate, EgressRate, priority`. Success = `SUCCESS` |

**GS116Ev2**: JGS form posts. Every page carries a fresh `secureRand`, which
must be read immediately before posting. Field order is `submitId … submitEnd`.

| Operation | Request |
|---|---|
| Read | `GET config/dot1based_advanced_vlan_membership.htm` (`vlanMem` = `vid?m1?…?m16` entries joined by `,`; `pvid`), `GET config/status_status.htm` (`portConfigEntry`) |
| VLAN add / delete | `POST config/dot1based_advanced_vlan_conf.htm` `submitId=vlanDot1VidCfg, secureRand, changeType=0, addVid=<vid>, confirmOK=0` / `…, changeType=0, delVid=<vid>` |
| Membership | `POST config/dot1based_advanced_vlan_membership.htm` `submitId=vlanDot1TagCfg, secureRand, vid, member, tag`. `member`/`tag` are bitmasks with bit *n* = port *n* (replaces the VLAN's whole membership) |
| PVID | `POST config/dot1based_advanced_pvid.htm` `submitId=vlanDot1PvidCfg, secureRand, port=<n>, pvid=<vid>` |
| Switch name | `POST config/status_switch_info.htm` `submitId=sysIp, secureRand, swName, dhcp=2` (DHCP on) or `dhcp=0, ip, subnet, gateway` (static, current values). This form also carries the management IP settings; they are echoed and checked afterwards |
| Speed/disable/name | `POST config/status_status.htm` `submitId=sysStatus, secureRand, speed (0 disable, 1 auto, 2/3 10 half/full, 4/5 100 half/full, 255 unchanged), flow (255 unchanged), description, port=<n>` |

## UniFi semantics worth knowing

- **"Allow All" is the UniFi default.** Every port is tagged in every network
  unless excluded. Adding a network in UniFi therefore makes the integration
  create that VLAN and tag it on every port, in one write per switch. On a
  production controller this means *all* your site's VLANs get tagged on
  every port; restrict them in the UniFi port editor (Block All / Custom) if
  unwanted.
- VLANs created in the NETGEAR web UI that UniFi does not know are deleted
  once writes are live (UniFi is authoritative), unless they are VLAN 1 or
  the management VLAN.
- A disabled port carries no VLAN exclusions in `system_cfg`, so UniFi's
  "Allow All" default would tag every network on it. The integration keeps a
  disabled port in its native VLAN only (untagged, nothing tagged): no stale
  VLANs, and nothing extra if the port is re-enabled on the switch itself.
  When it is re-enabled in UniFi, UniFi sends its real settings again.
- Forgetting the device in UniFi does **not** revert the switch. The ports
  keep their last configuration (a native switch would factory-reset).

## Verified on hardware (2026-10-29)

Live writes were tested on **ports 4 and 5 of both switches**, because the
switches carry a home network. That restriction was a test-harness measure,
not part of the product. First with the Python collector, then again with
the Go implementation:

- Go, end to end via UniFi: network creation (VLAN tagged on every port of
  both switches), access port in VLAN 3901 (join, PVID, leave VLAN 1, in that
  order), forced 100 full with a port name on both switches, disable
  (GS308Ev4), 10 half (GS116Ev2), switch rename on both (GS116Ev2 IP
  settings verified unchanged), drift on both switches corrected after a
  restart, revert to defaults and VLAN deletion.

Earlier, with the Python collector:

- Direct writer tests: VLAN create/edit/delete, access port in VLAN 3901
  (including removing port 4 from VLAN 1 while all other ports stayed
  untagged), PVID, forced 100 full (GS308Ev4) / 100 half (GS116Ev2), disable,
  auto and names. All verified by read-back and reverted.
- End to end via UniFi: network creation, access port (GS308Ev4), trunk with
  native 1, only VLAN 3902 tagged and 100 Mbit/s full (GS116Ev2), port
  disable, deletion of unused VLANs, revert to defaults. The controller showed
  provisioning, then connected/in sync.
- Drift: a manual change on the switch was corrected within the 5-minute
  re-check.
- Whole-switch mode in **dry run** against the real switches: the plan for
  the test controller's default configuration was empty (in sync) on all
  8/16 ports. Adding a network produced one write per switch tagging it on
  every port. Whole-switch mode was not applied live, to protect the home
  network.
