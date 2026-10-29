from __future__ import annotations

import logging

import pytest

from netgear_collector.model import DeviceInfo, PortReading, Reading
from netgear_collector.telemetry import RateCalculator

DEVICE = DeviceInfo(vendor="netgear", model="GS308Ev4", port_count=1, management_ip="192.0.2.1")


def reading(
    t: float, rx: int | None, tx: int | None, up: bool = True, speed: int = 1000
) -> Reading:
    port = PortReading(index=1, link_up=up, speed_mbps=speed, rx_bytes=rx, tx_bytes=tx)
    return Reading(device=DEVICE, ports=(port,), monotonic_time=t, wall_time=t + 1e9)


def test_first_reading_has_no_rate():
    snap = RateCalculator("sw").update(reading(0, 100, 200))
    port = snap.ports[0]
    assert (port.rx_bytes, port.tx_bytes) == (100, 200)
    assert port.rx_bytes_per_s is None
    assert port.tx_bytes_per_s is None


def test_rate_is_bytes_per_second():
    calc = RateCalculator("sw")
    calc.update(reading(0, 1_000, 2_000))
    port = calc.update(reading(10, 11_000, 52_000)).ports[0]
    assert port.rx_bytes_per_s == pytest.approx(1_000)
    assert port.tx_bytes_per_s == pytest.approx(5_000)


def test_counter_reset_is_detected_and_reported_without_rate(caplog):
    calc = RateCalculator("sw")
    calc.update(reading(0, 5_000_000, 5_000_000))
    with caplog.at_level(logging.INFO):
        snap = calc.update(reading(10, 1_000, 6_000_000))
    port = snap.ports[0]
    assert port.rx_bytes == 1_000  # raw counter passed through
    assert port.rx_bytes_per_s is None
    assert port.tx_bytes_per_s == pytest.approx(100_000)
    assert "counter reset detected" in caplog.text
    # The next interval after a reset has a rate again.
    assert calc.update(reading(20, 11_000, 6_000_000)).ports[0].rx_bytes_per_s == pytest.approx(
        1_000
    )


def test_rate_above_line_rate_is_dropped():
    calc = RateCalculator("sw")
    calc.update(reading(0, 0, 0, speed=100))
    # 100 Mbit/s = 12.5 MB/s; 50 MB/s in one second is a sampling glitch.
    port = calc.update(reading(1, 50_000_000, 1_000_000, speed=100)).ports[0]
    assert port.rx_bytes_per_s is None
    assert port.tx_bytes_per_s == pytest.approx(1_000_000)


def test_unknown_counters_stay_unknown():
    calc = RateCalculator("sw")
    calc.update(reading(0, None, None))
    port = calc.update(reading(10, None, None)).ports[0]
    assert port.rx_bytes is None
    assert port.rx_bytes_per_s is None


def test_non_increasing_time_yields_no_rate():
    calc = RateCalculator("sw")
    calc.update(reading(5, 0, 0))
    assert calc.update(reading(5, 100, 100)).ports[0].rx_bytes_per_s is None
