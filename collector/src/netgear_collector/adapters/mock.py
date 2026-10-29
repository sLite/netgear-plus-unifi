"""Simulated NETGEAR switches for development without hardware.

Port layout, speeds and traffic are derived from a seed so runs are
reproducible; counters grow with elapsed time so rates look realistic. An
optional flapping port and failure rate exercise link-state propagation and
the poller's error handling end to end.
"""

from __future__ import annotations

import copy
import random
import time
from collections.abc import Callable
from dataclasses import dataclass

from ..model import DeviceInfo, PortReading, Reading
from ..provisioning.model import Member, PortSettings, Speed, SwitchConfig
from ..provisioning.planner import Capabilities
from .base import AdapterError
from .netgear_write import ConfigWriter, advertised_speeds

MOCK_MODELS = {"GS308Ev4": (8, "V1.0.1.9"), "GS116Ev2": (16, "2.6.0.50")}
_SPEEDS = (1000, 1000, 100, 1000, 10)


@dataclass(frozen=True, slots=True)
class MockOptions:
    model: str = "GS308Ev4"
    seed: int = 0
    failure_rate: float = 0.0
    flap_port: int | None = None
    flap_period_seconds: float = 60.0


class MockAdapter:
    def __init__(
        self,
        switch_id: str,
        options: MockOptions,
        monotonic: Callable[[], float] = time.monotonic,
        wall: Callable[[], float] = time.time,
    ) -> None:
        if options.model not in MOCK_MODELS:
            raise ValueError(f"mock model must be one of {sorted(MOCK_MODELS)}")
        self._id = switch_id
        self._opts = options
        self._monotonic = monotonic
        self._wall = wall
        self._start = monotonic()
        self._failures = random.Random(options.seed)
        ports, firmware = MOCK_MODELS[options.model]
        layout = random.Random(options.seed)
        self._links = [layout.random() < 0.6 for _ in range(ports)]
        self._speeds = [layout.choice(_SPEEDS) for _ in range(ports)]
        self._rates = [layout.uniform(1e3, 2e6) for _ in range(ports)]
        # Counters integrate traffic over time so a port going down keeps
        # its totals, like real hardware.
        self._rx = [1_000_000.0 * (i + 1) for i in range(ports)]
        self._tx = [500_000.0 * (i + 1) for i in range(ports)]
        self._last = self._start
        self._config = SwitchConfig(
            vlans={1: dict.fromkeys(range(1, ports + 1), Member.UNTAGGED)},
            pvids=dict.fromkeys(range(1, ports + 1), 1),
            ports={p: PortSettings() for p in range(1, ports + 1)},
        )
        self._device = DeviceInfo(
            vendor="netgear",
            model=options.model,
            port_count=ports,
            management_ip="198.51.100.10",
            firmware=firmware,
            serial=f"MOCK{options.seed:09d}",
            port_speeds=advertised_speeds(MockConfigWriter.caps),
        )

    def read(self) -> Reading:
        if self._opts.failure_rate and self._failures.random() < self._opts.failure_rate:
            raise AdapterError("simulated switch timeout")
        now = self._monotonic()
        elapsed, dt = now - self._start, now - self._last
        self._last = now
        ports = tuple(self._port(i, elapsed, dt) for i in range(len(self._links)))
        return Reading(device=self._device, ports=ports, monotonic_time=now, wall_time=self._wall())

    def _port(self, i: int, elapsed: float, dt: float) -> PortReading:
        index = i + 1
        up = self._links[i] and self._config.ports[index].speed is not Speed.DISABLED
        if index == self._opts.flap_port:
            up = int(elapsed // (self._opts.flap_period_seconds / 2)) % 2 == 0
        if up:
            self._rx[i] += self._rates[i] * dt
            self._tx[i] += self._rates[i] * 0.4 * dt
        return PortReading(
            index=index,
            link_up=up,
            speed_mbps=self._speeds[i] if up else 0,
            full_duplex=True if up else None,
            autoneg=True,
            enabled=True,
            name=None,
            rx_bytes=int(self._rx[i]),
            tx_bytes=int(self._tx[i]),
            crc_errors=0,
        )

    def close(self) -> None:
        return None

    def config_writer(self) -> ConfigWriter:
        return MockConfigWriter(self._config, len(self._links))


class MockConfigWriter(ConfigWriter):
    """In-memory switch configuration with the real guard/verify logic."""

    caps = Capabilities(speeds=frozenset(Speed) - {Speed.M1000_FULL}, max_vlans=64)

    def __init__(self, config: SwitchConfig, port_count: int) -> None:
        super().__init__(None, "mock", port_count)
        self._config = config

    def read_config(self) -> SwitchConfig:
        return copy.deepcopy(self._config)

    def _create_vlan(self, vid: int, members: dict[int, Member], before: SwitchConfig) -> None:
        self._config.vlans[vid] = {}

    def _write_members(self, vid: int, members: dict[int, Member], before: SwitchConfig) -> None:
        self._config.vlans[vid] = dict(members)

    def _set_pvid(self, port: int, vid: int) -> None:
        self._config.pvids[port] = vid

    def _delete_vlan(self, vid: int) -> None:
        self._config.vlans.pop(vid, None)

    def _set_port(self, port: int, settings: PortSettings, before: SwitchConfig) -> None:
        self._config.ports[port] = PortSettings(settings.speed, settings.name or None)

    def _set_switch_name(self, name: str) -> None:
        self._config.switch_name = name or None
