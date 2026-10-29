"""Parse the switch section of UniFi's ``setparam.system_cfg``.

Observed on UniFi Network 10.6.109 (see docs/captures/):

    switch.vlan.<n>.id=<vid>                  VLAN table entry n
    switch.vlan.<n>.mode=tagged|untagged      membership of ports without an override
    switch.vlan.<n>.port.<p>.mode=untagged|tagged|exclude
    switch.port.<p>.pvid=<vid>                native VLAN (absent: 1)
    switch.port.<p>.status=disabled
    switch.port.<p>.autoneg=disabled, .speed=<mbps>, .duplex=enabled|disabled
    switch.port.<p>.name=<text>
    resolv.host.1.name=<hostname>             device name, hostname-safe (spaces removed)

Everything else in system_cfg (users, sshd, ntp, management VLAN, MTU...)
is ignored on purpose: it is not switch configuration and some of it must
never reach a switch.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field

from .model import Member, PortSettings, Speed, SwitchConfig

_VLAN = re.compile(r"^switch\.vlan\.(\d+)\.(id|mode)$")
_VLAN_PORT = re.compile(r"^switch\.vlan\.(\d+)\.port\.(\d+)\.mode$")
_PORT = re.compile(r"^switch\.port\.(\d+)\.(pvid|status|autoneg|speed|duplex|name)$")
_MODES = {"untagged": Member.UNTAGGED, "tagged": Member.TAGGED, "exclude": Member.EXCLUDED}
_FORCED = {
    (10, False): Speed.M10_HALF,
    (10, True): Speed.M10_FULL,
    (100, False): Speed.M100_HALF,
    (100, True): Speed.M100_FULL,
    (1000, True): Speed.M1000_FULL,
}


@dataclass(slots=True)
class ParsedUniFiConfig:
    config: SwitchConfig
    warnings: list[str] = field(default_factory=list)


_HOSTNAME_KEY = "resolv.host.1.name"


def switch_section(system_cfg: str) -> str:
    """The lines of a system_cfg document this integration uses, in order:
    ``switch.*`` and the device hostname."""
    return "\n".join(
        line.strip()
        for line in system_cfg.splitlines()
        if line.strip().startswith(("switch.", _HOSTNAME_KEY + "="))
    )


def parse_switch_config(text: str, port_count: int) -> ParsedUniFiConfig:
    """Resolve UniFi's VLAN defaults and per-port overrides into one
    explicit membership per (VLAN, port) for ports 1..port_count."""
    vlan_ids: dict[int, int] = {}
    vlan_modes: dict[int, Member] = {}
    overrides: dict[tuple[int, int], Member] = {}
    port_keys: dict[int, dict[str, str]] = {}
    hostname: str | None = None
    warnings: list[str] = []

    for raw in text.splitlines():
        key, sep, value = raw.strip().partition("=")
        if not sep:
            continue
        if m := _VLAN.match(key):
            n = int(m.group(1))
            if m.group(2) == "id" and value.isdigit():
                vlan_ids[n] = int(value)
            elif m.group(2) == "mode" and value in _MODES:
                vlan_modes[n] = _MODES[value]
        elif m := _VLAN_PORT.match(key):
            if value in _MODES:
                overrides[(int(m.group(1)), int(m.group(2)))] = _MODES[value]
        elif m := _PORT.match(key):
            port_keys.setdefault(int(m.group(1)), {})[m.group(2)] = value
        elif key == _HOSTNAME_KEY and value.strip():
            hostname = value.strip()

    config = SwitchConfig(switch_name=hostname)
    for n, vid in vlan_ids.items():
        default = vlan_modes.get(n, Member.TAGGED)
        members = {}
        for port in range(1, port_count + 1):
            member = overrides.get((n, port), default)
            if member is not Member.EXCLUDED:
                members[port] = member
        config.vlans[vid] = members

    for port in range(1, port_count + 1):
        keys = port_keys.get(port, {})
        pvid = keys.get("pvid", "1")
        config.pvids[port] = int(pvid) if pvid.isdigit() else 1
        config.ports[port] = PortSettings(
            speed=_speed(port, keys, warnings),
            name=_explicit_name(port, keys.get("name")),
        )
    return ParsedUniFiConfig(config, warnings)


def _speed(port: int, keys: dict[str, str], warnings: list[str]) -> Speed:
    if keys.get("status") == "disabled":
        return Speed.DISABLED
    if keys.get("autoneg") != "disabled":
        return Speed.AUTO
    try:
        mbps = int(keys.get("speed", ""))
    except ValueError:
        warnings.append(f"port {port}: autoneg off without a valid speed; using auto")
        return Speed.AUTO
    full = keys.get("duplex", "enabled") == "enabled"
    speed = _FORCED.get((mbps, full))
    if speed is None:
        warnings.append(f"port {port}: {mbps} Mbit/s {'full' if full else 'half'} unsupported")
        return Speed.AUTO
    return speed


def _explicit_name(port: int, name: str | None) -> str:
    """UniFi always sends a name; its default "Port N" means "no name", which
    clears the name on the switch for managed ports."""
    if name is None or name.strip() in ("", f"Port {port}"):
        return ""
    return name.strip()
