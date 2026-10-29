from __future__ import annotations

import pytest

from netgear_collector.adapters.mock import MockAdapter, MockConfigWriter, MockOptions
from netgear_collector.adapters.netgear_write import (
    ForbiddenWriteError,
    Gs30xWriter,
    JgsWriter,
    WriteError,
)
from netgear_collector.provisioning.model import Member, PortSettings, Speed
from netgear_collector.provisioning.planner import (
    CreateVlan,
    DeleteVlan,
    SetMembers,
    SetPort,
    SetSwitchName,
)

from .conftest import fixture_bytes

U, T, E = Member.UNTAGGED, Member.TAGGED, Member.EXCLUDED


def mock_writer(clock):
    return MockAdapter("m", MockOptions(model="GS308Ev4"), monotonic=clock).config_writer()


def test_guard_rejects_unsafe_operations(clock):
    writer = mock_writer(clock)
    with pytest.raises(ForbiddenWriteError, match="do not exist"):
        writer.apply(SetPort(9, PortSettings(Speed.DISABLED)))
    with pytest.raises(ForbiddenWriteError, match="VLAN 1"):
        writer.apply(DeleteVlan(1))
    with pytest.raises(ForbiddenWriteError, match="out of range"):
        writer.apply(CreateVlan(4095, {4: T}))
    writer.apply(CreateVlan(10, {8: T}))
    writer._config.management_vlan = 10
    with pytest.raises(ForbiddenWriteError, match="management"):
        writer.apply(DeleteVlan(10))
    assert writer.read_config().vlans[1] == dict.fromkeys(range(1, 9), U)


def test_membership_change_is_merged_into_fresh_state(clock):
    writer = mock_writer(clock)
    writer.apply(SetMembers(1, {4: E}))
    assert writer.read_config().vlans[1] == {p: U for p in (1, 2, 3, 5, 6, 7, 8)}


def test_emptying_a_vlan_is_refused(clock):
    writer = mock_writer(clock)
    writer.apply(CreateVlan(3901, {4: T}))
    with pytest.raises(ForbiddenWriteError, match="no members"):
        writer.apply(SetMembers(3901, {4: E}))


class ClumsyWriter(MockConfigWriter):
    """Simulates a firmware/encoding bug: the first write also drops port 1."""

    broken = True

    def _write_members(self, vid, members, before):
        if self.broken:
            self.broken = False
            members = {p: m for p, m in members.items() if p != 1}
        super()._write_members(vid, members, before)


def test_side_effects_on_other_ports_are_restored(clock):
    adapter = MockAdapter("m", MockOptions(model="GS308Ev4"), monotonic=clock)
    writer = ClumsyWriter(adapter._config, 8)
    with pytest.raises(WriteError, match="restored"):
        writer.apply(SetMembers(1, {4: E}))
    # Port 1 is back; the intended change (port 4 removed) stays.
    assert adapter._config.vlans[1] == {p: U for p in (1, 2, 3, 5, 6, 7, 8)}


class FakeFetcher:
    """Serves recorded pages and records POSTs (no network)."""

    def __init__(self, pages):
        self.pages = pages
        self.posts = []

    def request(self, method, url, data=None):
        path = url.split("/", 3)[3]
        response = type("R", (), {"status_code": 200})()
        if method == "post":
            self.posts.append((path, data))
            response.content = b"<div>ok</div>" if path != "port_status.cgi" else b"SUCCESS"
        else:
            response.content = self.pages[path]
        return response

    def has_ok_status(self, response):
        return response.status_code == 200


def fake_session(pages):
    fetcher = FakeFetcher(pages)
    connector = type("C", (), {"_page_fetcher": fetcher})()
    return type("S", (), {"_connector": connector})(), fetcher


def test_gs30x_reads_and_encodes_like_the_web_ui(monkeypatch):
    monkeypatch.setattr("netgear_collector.adapters.netgear_write._GS30X_PORT_SETTLE_SECONDS", 0)
    session, fetcher = fake_session(
        {
            "vlan.cgi": fixture_bytes("gs308ev4", "vlan_default"),
            "dashboard.cgi": fixture_bytes("gs308ev4", "info"),
        }
    )
    writer = Gs30xWriter(session, "192.0.2.1", 8)
    before = writer.read_config()
    assert before.vlans == {1: dict.fromkeys(range(1, 9), U)}
    assert before.ports[4] == PortSettings(Speed.AUTO, None)
    assert before.management_vlan == 1

    writer._write_members(1, {p: U for p in (1, 2, 3, 5, 6, 7, 8)}, before)
    writer._create_vlan(3901, {4: T}, before)
    writer._set_port(4, PortSettings(Speed.M100_FULL, "lab"), before)
    (p1, d1), (_, d2), (p3, d3) = fetcher.posts
    assert p1 == "8021qAdvanced.cgi"
    assert d1 == [
        ("hash", "1" * 32),
        ("ACTION", "Apply"),
        ("VLAN_ID", "1"),
        ("vlanNum", "1"),
        ("VLAN_NAME", "Default"),
        ("hiddenMem", "22232222"),
        ("voiceVLANID", "0"),
        ("voiceVlanCos", "6"),
    ]
    assert dict(d2)["ACTION"] == "Add" and dict(d2)["hiddenMem"] == "33313333"
    assert p3 == "port_status.cgi"
    assert dict(d3) == {
        "hash": "1" * 32,
        "port4": "checked",
        "SPEED": "6",
        "FLOW_CONTROL": "2",
        "DESCRIPTION": "lab",
        "IngressRate": "1",
        "EgressRate": "1",
        "priority": "0",
    }


def test_jgs_reads_and_encodes_like_the_web_ui():
    membership = fixture_bytes("gs116ev2", "vlan_membership")
    session, fetcher = fake_session(
        {
            "config/dot1based_advanced_vlan_membership.htm": membership,
            "config/dot1based_advanced_vlan_conf.htm": membership,  # only secureRand is used
            "config/dot1based_advanced_pvid.htm": membership,
            "config/status_status.htm": fixture_bytes("gs116ev2", "port_status"),
            "config/status_switch_info.htm": fixture_bytes("gs116ev2", "info"),
        }
    )
    writer = JgsWriter(session, "192.0.2.2", 16)
    before = writer.read_config()
    assert before.vlans == {1: dict.fromkeys(range(1, 17), U)}
    assert before.pvids == dict.fromkeys(range(1, 17), 1)
    assert before.ports[4] == PortSettings(Speed.AUTO, None)

    writer._write_members(3901, {4: T, 2: U}, before)
    writer._create_vlan(3901, {}, before)
    writer._set_pvid(4, 3901)
    writer._set_port(4, PortSettings(Speed.DISABLED, None), before)
    posts = [(path, dict(data)) for path, data in fetcher.posts]
    assert posts[0] == (
        "config/dot1based_advanced_vlan_membership.htm",
        {"submitId": "vlanDot1TagCfg", "secureRand": "ABCDEF01", "vid": "3901",
         "member": str((1 << 4) | (1 << 2)), "tag": str(1 << 4), "submitEnd": ""},
    )  # fmt: skip
    assert posts[1][1] == {
        "submitId": "vlanDot1VidCfg", "secureRand": "ABCDEF01", "changeType": "0",
        "addVid": "3901", "confirmOK": "0", "submitEnd": "",
    }  # fmt: skip
    assert posts[2][1]["port"] == "4" and posts[2][1]["pvid"] == "3901"
    assert posts[3][1] == {
        "submitId": "sysStatus", "secureRand": "ABCDEF02", "speed": "0", "flow": "255",
        "description": "", "port": "4", "submitEnd": "",
    }  # fmt: skip
    # Field order matters to the switch firmware: submitId first, submitEnd last.
    keys = [k for k, _ in fetcher.posts[0][1]]
    assert (keys[0], keys[-1]) == ("submitId", "submitEnd")
    assert all("changeType" not in d or d["changeType"] == "0" for _, d in posts)


def test_gs30x_switch_name_encoding():
    session, fetcher = fake_session(
        {
            "vlan.cgi": fixture_bytes("gs308ev4", "vlan_default"),
            "dashboard.cgi": fixture_bytes("gs308ev4", "info"),
        }
    )
    writer = Gs30xWriter(session, "192.0.2.1", 8)
    assert writer.read_config().switch_name is None
    fetcher.request = _answer(fetcher, b"SWITCH NAME SUCCESS")
    writer._set_switch_name("SwitchKeller")
    assert fetcher.posts == [
        (
            "switch_name.cgi",
            [("hash", "1" * 32), ("switch_name", "SwitchKeller"), ("SET_LANG", "en")],
        )
    ]


def _answer(fetcher, body):
    original = fetcher.request

    def request(method, url, data=None):
        response = original(method, url, data)
        if method == "post":
            response.content = body
        return response

    return request


def _jgs_session(info_pages):
    pages = iter(info_pages)
    session, fetcher = fake_session({})
    original = fetcher.request

    def request(method, url, data=None):
        if url.endswith("status_switch_info.htm") and method == "get":
            fetcher.pages["config/status_switch_info.htm"] = next(pages)
        return original(method, url, data)

    fetcher.request = request
    return session, fetcher


def test_jgs_switch_name_echoes_ip_settings_unchanged():
    info = fixture_bytes("gs116ev2", "info")
    session, fetcher = _jgs_session([info, info])
    JgsWriter(session, "192.0.2.2", 16)._set_switch_name("SwitchBuero")
    path, data = fetcher.posts[0]
    assert path == "config/status_switch_info.htm"
    assert [k for k, _ in data] == ["submitId", "secureRand", "swName", "dhcp", "submitEnd"]
    assert dict(data)["swName"] == "SwitchBuero" and dict(data)["dhcp"] == "2"  # DHCP stays on


def test_jgs_switch_name_detects_changed_ip_settings():
    info = fixture_bytes("gs116ev2", "info")
    changed = info.replace(b"?Enable?192.168.1.230?", b"?Enable?192.168.1.99?")
    session, _ = _jgs_session([info, changed])
    with pytest.raises(WriteError, match="IP settings changed"):
        JgsWriter(session, "192.0.2.2", 16)._set_switch_name("SwitchBuero")


def test_switch_name_guard_and_verify(clock):
    writer = mock_writer(clock)
    with pytest.raises(ForbiddenWriteError, match="not valid"):
        writer.apply(SetSwitchName("x" * 21))
    writer.apply(SetSwitchName("SwitchKeller"))
    assert writer.read_config().switch_name == "SwitchKeller"
