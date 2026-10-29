from __future__ import annotations

import pytest

from netgear_collector.adapters import netgear_pages as pages

from .conftest import fixture_bytes


def test_gs308ev4_counters_from_real_page():
    counters = pages.gs108ev4_counters(fixture_bytes("gs308ev4", "stats"))
    assert sorted(counters) == list(range(1, 9))
    assert counters[1] == pages.PortCounters(rx_bytes=1866737, tx_bytes=8704593, crc_errors=0)
    assert counters[8] == pages.PortCounters(0, 0, 0)


def test_gs308ev4_counters_combine_high_word():
    """py-netgear-plus reads only the low word; the high word must be added."""
    page = fixture_bytes("gs308ev4", "stats").decode()
    # Port 1 rows: rx_hi, rx_lo, tx_hi, tx_lo -> set rx_hi=25 and tx_hi=2.
    page = page.replace(
        '<input type="hidden" value="0">\n<input type="hidden" value="1866737">\n'
        '<input type="hidden" value="0">\n<input type="hidden" value="8704593">',
        '<input type="hidden" value="25">\n<input type="hidden" value="1866737">\n'
        '<input type="hidden" value="2">\n<input type="hidden" value="8704593">',
        1,
    )
    counters = pages.gs108ev4_counters(page.encode())
    assert counters[1].rx_bytes == 25 * 2**32 + 1866737
    assert counters[1].tx_bytes == 2 * 2**32 + 8704593


def test_gs308ev4_counters_tolerate_missing_markup():
    assert (
        pages.gs108ev4_counters(b"<html><body><ul><li><span>1</span></li></ul></body></html>") == {}
    )


def test_gs308ev4_port_extras():
    info = fixture_bytes("gs308ev4", "info")
    extras = pages.gs108ev4_port_extras(info)
    assert extras[1] == pages.PortExtras(full_duplex=True, name=None)
    assert extras[2] == pages.PortExtras(full_duplex=None, name=None)
    assert pages.gs108ev4_hardware_mac(info) == "28:94:01:00:00:01"


def test_jgs_hardware_mac():
    assert pages.jgs_hardware_mac(fixture_bytes("gs116ev2", "info")) == "38:94:ed:00:00:02"
    assert pages.jgs_hardware_mac(b"<html></html>") is None


@pytest.mark.parametrize(
    ("text", "mbps"),
    [
        ("1000M full", 1000),
        ("1000M", 1000),
        ("100M half", 100),
        ("10M", 10),
        ("2.5G", 2500),
        ("No Speed", 0),
        ("weird", None),
        (None, None),
    ],
)
def test_parse_link_speed(text, mbps):
    assert pages.parse_link_speed(text) == mbps


@pytest.mark.parametrize(
    ("text", "duplex"),
    [("1000M full", True), ("10M half", False), ("1000M", None), ("", None)],
)
def test_parse_duplex(text, duplex):
    assert pages.parse_duplex(text) is duplex


def test_is_login_redirect():
    assert pages.is_login_redirect(b"<script> parent.location.href='/login.htm'; </script>")
    assert pages.is_login_redirect(b'<script>top.location.href = "/wmi/login"</script>')
    assert not pages.is_login_redirect(fixture_bytes("gs116ev2", "status"))
    assert not pages.is_login_redirect(fixture_bytes("gs308ev4", "stats"))
