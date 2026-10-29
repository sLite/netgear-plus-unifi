from __future__ import annotations

import ast
import inspect

import pytest
from py_netgear_plus.fetcher import LoginFailedError, MaxSessionsError, PageNotLoadedError

from netgear_collector.adapters import netgear_plus
from netgear_collector.adapters.base import (
    AdapterError,
    AuthenticationError,
    IdentityMismatchError,
    SessionDisplacedError,
    SessionLimitError,
)
from netgear_collector.adapters.netgear_plus import NetgearPlusAdapter, Page

from .conftest import FakeSession


def make_adapter(session: FakeSession, clock, expected: str | None = None) -> NetgearPlusAdapter:
    return NetgearPlusAdapter(
        "sw",
        "192.0.2.10",
        "secret",
        expected_model=expected,
        session_factory=lambda host, pw: session,
        monotonic=clock,
        wall=clock,
    )


def test_gs308ev4_reading(clock):
    session = FakeSession("GS308Ev4")
    reading = make_adapter(session, clock, "GS308Ev4").read()

    assert reading.device.model == "GS308Ev4"
    assert reading.device.firmware == "V1.0.1.9"
    assert reading.device.serial == "SERIAL308TEST"
    assert reading.device.port_speeds == ("auto", "10f", "100f", "1000f")
    assert reading.device.hardware_mac == "28:94:01:00:00:01"
    assert reading.device.port_count == len(reading.ports) == 8
    p1 = reading.ports[0]
    assert (p1.link_up, p1.speed_mbps, p1.full_duplex, p1.autoneg, p1.enabled) == (
        True, 1000, True, True, True,
    )  # fmt: skip
    assert (p1.rx_bytes, p1.tx_bytes, p1.crc_errors) == (1866737, 8704593, 0)
    assert all(not p.link_up and p.speed_mbps == 0 for p in reading.ports[1:])
    # Dashboard doubles as status page: fetched once, plus statistics.
    assert session.fetches == [Page.SWITCH_INFO, Page.PORT_STATISTICS]


def test_gs116ev2_reading(clock):
    reading = make_adapter(FakeSession("GS116Ev2"), clock, "GS116Ev2").read()

    assert reading.device.firmware == "2.6.0.50"
    assert reading.device.port_speeds == ("auto", "10h", "10f", "100h", "100f", "1000f")
    assert reading.device.hardware_mac == "38:94:ed:00:00:02"
    assert len(reading.ports) == 16
    up = {p.index: p.speed_mbps for p in reading.ports if p.link_up}
    assert up == {1: 10, 2: 1000, 7: 1000, 8: 1000, 12: 100, 15: 1000}
    p8 = reading.ports[7]
    assert p8.rx_bytes == 24774038874  # > 2**32: full 64-bit decimal counter
    assert p8.full_duplex is None  # GS116Ev2 does not report duplex


def test_metadata_is_cached_between_polls(clock):
    session = FakeSession("GS116Ev2")
    adapter = make_adapter(session, clock)
    adapter.read()
    adapter.read()
    assert session.fetches.count(Page.SWITCH_INFO) == 1
    clock.advance(NetgearPlusAdapter.METADATA_REFRESH_SECONDS + 1)
    adapter.read()
    assert session.fetches.count(Page.SWITCH_INFO) == 2
    assert session.logins == 1


def test_model_mismatch_is_refused(clock):
    session = FakeSession("GS116Ev2")
    with pytest.raises(IdentityMismatchError):
        make_adapter(session, clock, expected="GS308Ev4").read()
    assert session.logins == 0


def test_login_rejected_maps_to_authentication_error(clock):
    session = FakeSession("GS308Ev4")
    session.login_error = LoginFailedError("bad password")
    with pytest.raises(AuthenticationError):
        make_adapter(session, clock).read()


def test_max_sessions_maps_to_session_limit(clock):
    session = FakeSession("GS308Ev4")
    session.login_error = MaxSessionsError("Maximum number of sessions")
    with pytest.raises(SessionLimitError):
        make_adapter(session, clock).read()


def test_fetch_failure_drops_session_and_reconnects(clock):
    sessions = [FakeSession("GS308Ev4"), FakeSession("GS308Ev4")]
    sessions[0].fetch_error = PageNotLoadedError("timeout")
    created = []

    def factory(host, pw):
        created.append(sessions[len(created)])
        return created[-1]

    adapter = NetgearPlusAdapter("sw", "192.0.2.10", "pw", session_factory=factory, monotonic=clock)
    with pytest.raises(AdapterError, match="PageNotLoadedError"):
        adapter.read()
    assert sessions[0].logouts == 1
    assert adapter.read().ports[0].link_up
    assert len(created) == 2


def test_port_count_mismatch_is_an_adapter_error(clock):
    truncated = b"<html><script>portConfigEntry[0] = '1??Up?Auto?1000M?Disable';</script></html>"
    session = FakeSession("GS116Ev2", overrides={Page.PORT_STATUS: truncated})
    with pytest.raises(AdapterError):
        make_adapter(session, clock).read()


def test_close_logs_out(clock):
    session = FakeSession("GS308Ev4")
    adapter = make_adapter(session, clock)
    adapter.read()
    adapter.close()
    adapter.close()
    assert session.logouts == 1


def test_read_only_session_cannot_reach_write_operations():
    """Safety regression test: no write path of the library may be referenced."""
    assert {p.value for p in Page} == {
        "SWITCH_INFO_TEMPLATES",
        "PORT_STATUS_TEMPLATES",
        "PORT_STATISTICS_TEMPLATES",
    }
    tree = ast.parse(inspect.getsource(netgear_plus))
    docstrings = {
        id(node.body[0].value)
        for node in ast.walk(tree)
        if isinstance(node, ast.Module | ast.ClassDef | ast.FunctionDef)
        and node.body
        and isinstance(node.body[0], ast.Expr)
        and isinstance(node.body[0].value, ast.Constant)
    }
    used = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Attribute):
            used.add(node.attr)
        elif isinstance(node, ast.Name):
            used.add(node.id)
        elif (
            isinstance(node, ast.Constant)
            and isinstance(node.value, str)
            and id(node) not in docstrings
        ):
            used.add(node.value)
    forbidden = (
        "switch_port", "turn_on", "turn_off", "power_cycle", "reboot", "set_vlan",
        "add_vlan", "edit_vlan", "remove_vlan", "apply_vlan_config", "set_port_name",
        "set_ip_config", "change_password", "switch_leds", "switch_poe_port",
        "_SET_TEMPLATES", "REBOOT", "SWITCH_PORT_TEMPLATES", "POE_PORT",
    )  # fmt: skip
    assert sorted(u for u in used for f in forbidden if f.lower() in u.lower()) == []


# What a GS116Ev2 returns to a session displaced by another login (captured).
DISPLACED_PAGE = (
    b"<html><body><script language=JavaScript> parent.location.href='/login.htm'; "
    b"</script></body></html>"
)


def test_displaced_session_is_reported_distinctly(clock):
    session = FakeSession("GS116Ev2", overrides={Page.PORT_STATUS: DISPLACED_PAGE})
    with pytest.raises(SessionDisplacedError):
        make_adapter(session, clock).read()
    assert session.logouts == 1
