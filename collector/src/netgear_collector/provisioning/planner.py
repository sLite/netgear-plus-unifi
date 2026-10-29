"""Diff desired against actual configuration into an ordered plan.

UniFi is authoritative for every port of a switch with writes enabled, as
for a native UniFi switch. Safety properties (unit-tested):

* VLAN 1 and the switch's management VLAN are never deleted.
* VLANs that UniFi does not define, or that would be left without ports,
  are deleted (the switches cannot hold an empty 802.1Q VLAN).
* Membership operations carry only the ports whose membership changes; the
  writer merges them into a fresh read of the switch.
* Ordering keeps ports connected throughout: add memberships, then move
  PVIDs, then remove memberships and delete VLANs, then change
  speed/admin state/names.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass, field

from .model import Member, PortSettings, Speed, SwitchConfig

DEFAULT_VLAN = 1


@dataclass(frozen=True, slots=True)
class CreateVlan:
    vid: int
    members: dict[int, Member]

    def __str__(self) -> str:
        return f"create VLAN {self.vid} with {_fmt(self.members)}"


@dataclass(frozen=True, slots=True)
class SetMembers:
    vid: int
    members: dict[int, Member]

    def __str__(self) -> str:
        return f"VLAN {self.vid}: set {_fmt(self.members)}"


@dataclass(frozen=True, slots=True)
class SetPvid:
    port: int
    vid: int

    def __str__(self) -> str:
        return f"port {self.port}: PVID {self.vid}"


@dataclass(frozen=True, slots=True)
class DeleteVlan:
    vid: int

    def __str__(self) -> str:
        return f"delete VLAN {self.vid}"


@dataclass(frozen=True, slots=True)
class SetPort:
    port: int
    settings: PortSettings

    def __str__(self) -> str:
        name = f", name {self.settings.name!r}" if self.settings.name is not None else ""
        return f"port {self.port}: speed {self.settings.speed.value}{name}"


@dataclass(frozen=True, slots=True)
class SetSwitchName:
    name: str

    def __str__(self) -> str:
        return f"switch name {self.name!r}"


Operation = CreateVlan | SetMembers | SetPvid | DeleteVlan | SetPort | SetSwitchName

# Both NETGEAR UIs accept at most 20 printable ASCII characters.
SWITCH_NAME_MAX = 20


@dataclass(frozen=True, slots=True)
class Capabilities:
    speeds: frozenset[Speed]
    max_vlans: int


@dataclass(slots=True)
class Plan:
    operations: list[Operation] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)

    def describe(self) -> list[str]:
        return [str(op) for op in self.operations]


def _fmt(members: dict[int, Member]) -> str:
    return ", ".join(f"port {p}={m.value}" for p, m in sorted(members.items()))


def protected_vlans(actual: SwitchConfig) -> set[int]:
    """VLANs that are never deleted: the default and the management VLAN."""
    return {DEFAULT_VLAN, actual.management_vlan}


def plan(
    desired: SwitchConfig,
    actual: SwitchConfig,
    ports: Iterable[int],
    caps: Capabilities,
) -> Plan:
    """Plan the changes that make ``ports`` of the switch match ``desired``."""
    managed = sorted(set(ports))
    protected = protected_vlans(actual)
    result = Plan()

    vlan_count = len(actual.vlans)
    for vid in sorted(set(desired.vlans) | set(actual.vlans)):
        gains = {
            p: desired.member(vid, p)
            for p in managed
            if desired.member(vid, p) is not Member.EXCLUDED
            and desired.member(vid, p) is not actual.member(vid, p)
        }
        if not gains:
            continue
        if vid in actual.vlans:
            result.operations.append(SetMembers(vid, gains))
        elif vlan_count >= caps.max_vlans:
            result.warnings.append(f"VLAN {vid} not created: switch limit of {caps.max_vlans}")
        else:
            vlan_count += 1
            result.operations.append(CreateVlan(vid, gains))

    for port in managed:
        want = desired.pvids.get(port, DEFAULT_VLAN)
        if want == actual.pvids.get(port, DEFAULT_VLAN):
            continue
        if desired.member(want, port) is Member.EXCLUDED:
            result.warnings.append(f"port {port}: PVID {want} skipped, port is not a member")
            continue
        result.operations.append(SetPvid(port, want))

    for vid in sorted(actual.vlans):
        current = actual.vlans[vid]
        losses = {
            p: Member.EXCLUDED
            for p in managed
            if p in current and desired.member(vid, p) is Member.EXCLUDED
        }
        remaining = [p for p in current if p not in losses]
        if vid not in protected and (vid not in desired.vlans or not remaining):
            result.operations.append(DeleteVlan(vid))
        elif losses and remaining:
            result.operations.append(SetMembers(vid, losses))
        elif losses:
            result.warnings.append(
                f"VLAN {vid}: kept (default/management VLAN) although UniFi wants it without ports"
            )

    for port in managed:
        result.operations.extend(_port_ops(port, desired, actual, caps, result.warnings))

    if desired.switch_name is not None:
        name = switch_name(desired.switch_name)
        if name != (actual.switch_name or ""):
            result.operations.append(SetSwitchName(name))
    return result


def switch_name(name: str) -> str:
    """The name as the switches accept it: printable ASCII, 20 characters."""
    return "".join(c for c in name if " " <= c <= "~")[:SWITCH_NAME_MAX]


def _port_ops(
    port: int, desired: SwitchConfig, actual: SwitchConfig, caps: Capabilities, warnings: list[str]
) -> list[Operation]:
    want = desired.ports.get(port, PortSettings())
    have = actual.ports.get(port, PortSettings())
    speed = want.speed
    if speed is Speed.M1000_FULL and speed not in caps.speeds and Speed.AUTO in caps.speeds:
        # Neither switch can force 1000 Mbit/s; 1000BASE-T needs
        # autonegotiation anyway, so auto is the closest equivalent.
        speed = Speed.AUTO
    elif speed not in caps.speeds:
        warnings.append(f"port {port}: speed {speed.value} not supported by this switch; unchanged")
        speed = have.speed
    name_change = want.name is not None and want.name != (have.name or "")
    if speed is have.speed and not name_change:
        return []
    name = want.name if want.name is not None else have.name
    return [SetPort(port, PortSettings(speed=speed, name=name))]
