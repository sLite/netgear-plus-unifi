"""Turns successive raw readings into snapshots with traffic rates.

Counters are passed through exactly as the switch reports them (bytes since
the switch booted or its statistics were cleared), the same semantics a real
UniFi switch reports. Rates are derived here:

* the first reading of a port has no rate;
* a counter that goes backwards is a reset (reboot or "clear counters"): the
  rate for that interval is unknown and the reset is counted;
* a rate above what the negotiated link speed allows is a sampling glitch and
  is dropped rather than reported.
"""

from __future__ import annotations

import logging

from .model import PortReading, PortTelemetry, Reading, SwitchSnapshot

_LOGGER = logging.getLogger(__name__)

# Headroom over line rate: the switch updates counters asynchronously to our
# HTTP requests, so a short interval can legitimately overshoot a little.
LINE_RATE_TOLERANCE = 1.25
# Used when the link speed is unknown: nothing on these switches exceeds 10G.
FALLBACK_MAX_MBPS = 10_000


class RateCalculator:
    """Per-switch state for deriving rates. Not thread-safe; one per poller."""

    def __init__(self, switch_id: str) -> None:
        self._switch_id = switch_id
        self._previous: Reading | None = None

    def update(self, reading: Reading) -> SwitchSnapshot:
        previous = self._previous
        prev_ports = {p.index: p for p in previous.ports} if previous else {}
        dt = reading.monotonic_time - previous.monotonic_time if previous else 0.0
        ports = tuple(self._port(p, prev_ports.get(p.index), dt) for p in reading.ports)
        self._previous = reading
        return SwitchSnapshot(
            device=reading.device,
            ports=ports,
            collected_at=reading.wall_time,
            monotonic_time=reading.monotonic_time,
        )

    def _port(self, cur: PortReading, prev: PortReading | None, dt: float) -> PortTelemetry:
        max_mbps = cur.speed_mbps if cur.link_up and cur.speed_mbps else FALLBACK_MAX_MBPS
        prev_rx = prev.rx_bytes if prev else None
        prev_tx = prev.tx_bytes if prev else None
        rx_rate = self._rate(cur.index, "rx", prev_rx, cur.rx_bytes, dt, max_mbps)
        tx_rate = self._rate(cur.index, "tx", prev_tx, cur.tx_bytes, dt, max_mbps)
        return PortTelemetry(
            index=cur.index,
            link_up=cur.link_up,
            speed_mbps=cur.speed_mbps,
            full_duplex=cur.full_duplex,
            autoneg=cur.autoneg,
            enabled=cur.enabled,
            name=cur.name,
            rx_bytes=cur.rx_bytes,
            tx_bytes=cur.tx_bytes,
            rx_bytes_per_s=rx_rate,
            tx_bytes_per_s=tx_rate,
            crc_errors=cur.crc_errors,
        )

    def _rate(
        self,
        port: int,
        direction: str,
        prev: int | None,
        cur: int | None,
        dt: float,
        max_mbps: int,
    ) -> float | None:
        if prev is None or cur is None or dt <= 0:
            return None
        if cur < prev:
            _LOGGER.info(
                "counter reset detected switch=%s port=%d direction=%s (%d -> %d)",
                self._switch_id,
                port,
                direction,
                prev,
                cur,
            )
            return None
        rate = (cur - prev) / dt
        max_rate = max_mbps * 1_000_000 / 8 * LINE_RATE_TOLERANCE
        if rate > max_rate:
            _LOGGER.debug(
                "implausible rate dropped switch=%s port=%d direction=%s rate=%.0f B/s",
                self._switch_id,
                port,
                direction,
                rate,
            )
            return None
        return rate
