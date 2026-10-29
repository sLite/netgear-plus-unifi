"""Vendor-independent switch configuration model for provisioning."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum


class Member(StrEnum):
    """Membership of a port in an 802.1Q VLAN."""

    UNTAGGED = "U"
    TAGGED = "T"
    EXCLUDED = "E"


class Speed(StrEnum):
    """Administrative port speed setting. DISABLED is how NETGEAR Plus
    switches (and this model) express "port administratively down"."""

    AUTO = "auto"
    DISABLED = "disabled"
    M10_HALF = "10h"
    M10_FULL = "10f"
    M100_HALF = "100h"
    M100_FULL = "100f"
    M1000_FULL = "1000f"


@dataclass(frozen=True, slots=True)
class PortSettings:
    speed: Speed = Speed.AUTO
    # Desired: "" clears the name, None leaves it alone. Actual: None = unnamed.
    name: str | None = None


@dataclass(slots=True)
class SwitchConfig:
    """VLANs, PVIDs and port settings of one switch (desired or actual).

    ``vlans`` maps VLAN id to the ports that are members; a port missing
    from a VLAN's map is excluded from it.
    """

    vlans: dict[int, dict[int, Member]] = field(default_factory=dict)
    pvids: dict[int, int] = field(default_factory=dict)
    ports: dict[int, PortSettings] = field(default_factory=dict)
    # Actual config only: the VLAN the switch's own management is reachable in.
    management_vlan: int = 1
    # Switch (system) name. Desired: None leaves it alone.
    switch_name: str | None = None

    def member(self, vid: int, port: int) -> Member:
        return self.vlans.get(vid, {}).get(port, Member.EXCLUDED)
