"""Vendor-independent switch telemetry model.

Adapters produce ``Reading`` objects with raw counters; the telemetry layer
turns successive readings into ``SwitchSnapshot`` objects with rates. Nothing
here is NETGEAR specific. ``None`` always means "the switch does not report
this", never zero.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
from enum import StrEnum
from typing import Any


@dataclass(frozen=True, slots=True)
class DeviceInfo:
    """Identity of a physical switch. Never used as the virtual UniFi identity."""

    vendor: str
    model: str
    port_count: int
    management_ip: str
    firmware: str | None = None
    serial: str | None = None
    # The switch's own (burned-in) MAC, as the rest of the network sees it.
    hardware_mac: str | None = None
    # Speed settings every port accepts (values of provisioning.model.Speed),
    # so the virtual device can advertise exactly these to UniFi. Empty when
    # the model has no write support.
    port_speeds: tuple[str, ...] = ()


@dataclass(frozen=True, slots=True)
class PortReading:
    """One port as read from the switch, with raw cumulative counters."""

    index: int
    link_up: bool
    speed_mbps: int | None = None
    full_duplex: bool | None = None
    autoneg: bool | None = None
    enabled: bool | None = None
    name: str | None = None
    rx_bytes: int | None = None
    tx_bytes: int | None = None
    crc_errors: int | None = None


@dataclass(frozen=True, slots=True)
class Reading:
    """One complete poll of a switch."""

    device: DeviceInfo
    ports: tuple[PortReading, ...]
    monotonic_time: float
    wall_time: float


@dataclass(frozen=True, slots=True)
class PortTelemetry:
    """A port with rates derived from successive readings (bytes per second)."""

    index: int
    link_up: bool
    speed_mbps: int | None
    full_duplex: bool | None
    autoneg: bool | None
    enabled: bool | None
    name: str | None
    rx_bytes: int | None
    tx_bytes: int | None
    rx_bytes_per_s: float | None
    tx_bytes_per_s: float | None
    crc_errors: int | None


@dataclass(frozen=True, slots=True)
class SwitchSnapshot:
    device: DeviceInfo
    ports: tuple[PortTelemetry, ...]
    collected_at: float
    monotonic_time: float

    def to_json(self) -> dict[str, Any]:
        data = asdict(self)
        del data["monotonic_time"]
        return data


class CollectorState(StrEnum):
    STARTING = "starting"
    OK = "ok"
    UNAVAILABLE = "unavailable"
