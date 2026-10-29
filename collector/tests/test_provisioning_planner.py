from __future__ import annotations

from pathlib import Path

from netgear_collector.provisioning.model import Member, PortSettings, Speed, SwitchConfig
from netgear_collector.provisioning.planner import (
    Capabilities,
    CreateVlan,
    DeleteVlan,
    SetMembers,
    SetPort,
    SetPvid,
    SetSwitchName,
    plan,
)
from netgear_collector.provisioning.unifi_cfg import parse_switch_config, switch_section

CAPTURE = Path(__file__).parents[2] / "docs" / "captures" / "system_cfg-switch-section.txt"
CAPS = Capabilities(speeds=frozenset(Speed) - {Speed.M1000_FULL}, max_vlans=64)
U, T, E = Member.UNTAGGED, Member.TAGGED, Member.EXCLUDED


def factory_default(ports: int) -> SwitchConfig:
    return SwitchConfig(
        vlans={1: {p: U for p in range(1, ports + 1)}},
        pvids={p: 1 for p in range(1, ports + 1)},
        ports={p: PortSettings() for p in range(1, ports + 1)},
    )


def test_parse_captured_controller_config():
    text = switch_section(CAPTURE.read_text())
    parsed = parse_switch_config(text, 16)
    cfg = parsed.config
    assert sorted(cfg.vlans) == [1, 20, 30]
    # port 3: access port in VLAN 20
    assert (cfg.member(1, 3), cfg.member(20, 3), cfg.member(30, 3)) == (E, U, E)
    assert cfg.pvids[3] == 20
    # port 4: trunk, native default, tagged 20, excluded 30
    assert (cfg.member(1, 4), cfg.member(20, 4), cfg.member(30, 4)) == (U, T, E)
    # port 1: UniFi default "allow all" = untagged 1, tagged everything else
    assert (cfg.member(1, 1), cfg.member(20, 1), cfg.member(30, 1)) == (U, T, T)
    assert cfg.ports[5].speed is Speed.DISABLED
    assert cfg.ports[6].speed is Speed.M100_FULL
    assert cfg.ports[3].name == "access-vlan20"
    assert cfg.ports[1].name == ""  # "Port 1" is UniFi's default: unnamed
    assert parsed.warnings == []


def test_unsupported_forced_speed_is_a_warning():
    text = "switch.port.2.autoneg=disabled\nswitch.port.2.speed=2500\nswitch.port.2.duplex=enabled"
    parsed = parse_switch_config(text, 4)
    assert parsed.config.ports[2].speed is Speed.AUTO
    assert "2500" in parsed.warnings[0]


ALL8 = range(1, 9)


def test_access_port_plan_is_ordered():
    actual = factory_default(8)
    desired = factory_default(8)
    desired.vlans[3901] = {4: U}
    desired.vlans[1].pop(4)
    desired.pvids[4] = 3901

    ops = plan(desired, actual, ALL8, CAPS).operations
    assert ops == [CreateVlan(3901, {4: U}), SetPvid(4, 3901), SetMembers(1, {4: E})]


def test_every_port_is_managed():
    actual = factory_default(8)
    desired = factory_default(8)
    desired.vlans[3901] = dict.fromkeys(ALL8, T)  # UniFi "allow all" on every port
    desired.ports[2] = PortSettings(speed=Speed.DISABLED)
    ops = plan(desired, actual, ALL8, CAPS).operations
    assert ops == [
        CreateVlan(3901, dict.fromkeys(ALL8, T)),
        SetPort(2, PortSettings(Speed.DISABLED, None)),
    ]


def test_vlans_unknown_to_unifi_or_left_empty_are_deleted():
    actual = factory_default(8)
    actual.vlans[1234] = {8: T}  # created in the NETGEAR web UI, unknown to UniFi
    actual.vlans[3901] = {4: T}
    desired = factory_default(8)
    desired.vlans[3901] = {}  # network exists in UniFi but no port uses it
    ops = plan(desired, actual, ALL8, CAPS).operations
    assert ops == [DeleteVlan(1234), DeleteVlan(3901)]


def test_default_and_management_vlan_are_never_deleted():
    actual = factory_default(8)
    actual.vlans[10] = {8: T}
    actual.management_vlan = 10
    desired = factory_default(8)
    desired.vlans[1] = {}
    desired.vlans[3901] = dict.fromkeys(ALL8, U)
    desired.pvids = dict.fromkeys(ALL8, 3901)
    result = plan(desired, actual, ALL8, CAPS)
    assert DeleteVlan(1) not in result.operations
    assert DeleteVlan(10) not in result.operations
    assert len(result.warnings) == 2


def test_trunk_with_custom_tagged_vlans():
    actual = factory_default(8)
    actual.vlans[3901] = {4: T}
    actual.vlans[3902] = {4: T, 5: T}
    desired = factory_default(8)
    desired.vlans[3901] = {}
    desired.vlans[3902] = {4: T, 5: T}
    desired.vlans[3903] = {4: T}
    ops = plan(desired, actual, ALL8, CAPS).operations
    assert ops == [CreateVlan(3903, {4: T}), DeleteVlan(3901)]


def test_pvid_needs_membership():
    desired = factory_default(8)
    desired.pvids[4] = 3901  # but port 4 not member of 3901
    result = plan(desired, factory_default(8), ALL8, CAPS)
    assert result.operations == []
    assert "not a member" in result.warnings[0]


def test_port_settings():
    desired = factory_default(8)
    desired.ports[4] = PortSettings(speed=Speed.M100_FULL, name="lab")
    assert plan(desired, factory_default(8), ALL8, CAPS).operations == [
        SetPort(4, PortSettings(Speed.M100_FULL, "lab"))
    ]
    desired.ports[4] = PortSettings(speed=Speed.M10_HALF)
    gs308 = Capabilities(speeds=CAPS.speeds - {Speed.M10_HALF}, max_vlans=64)
    result = plan(desired, factory_default(8), ALL8, gs308)
    assert result.operations == []
    assert "not supported" in result.warnings[0]


def test_forced_gigabit_means_auto():
    actual = factory_default(8)
    actual.ports[4] = PortSettings(speed=Speed.M100_FULL)
    desired = factory_default(8)
    desired.ports[4] = PortSettings(speed=Speed.M1000_FULL)
    result = plan(desired, actual, ALL8, CAPS)
    assert result.operations == [SetPort(4, PortSettings(Speed.AUTO, None))]
    assert result.warnings == []
    assert plan(desired, factory_default(8), ALL8, CAPS).operations == []


def test_vlan_limit():
    desired = factory_default(8)
    desired.vlans[3901] = {4: T}
    caps = Capabilities(speeds=CAPS.speeds, max_vlans=1)
    result = plan(desired, factory_default(8), ALL8, caps)
    assert result.operations == []
    assert "limit" in result.warnings[0]


def test_in_sync_yields_empty_plan():
    assert plan(factory_default(8), factory_default(8), ALL8, CAPS).operations == []


def test_default_unifi_name_clears_switch_name():
    actual = factory_default(8)
    actual.ports[4] = PortSettings(name="old-label")
    text = "switch.vlan.1.id=1\nswitch.vlan.1.mode=untagged\nswitch.port.4.name=Port 4"
    desired = parse_switch_config(text, 8).config
    assert plan(desired, actual, ALL8, CAPS).operations == [
        SetPort(4, PortSettings(Speed.AUTO, ""))
    ]
    actual.ports[4] = PortSettings()
    assert plan(desired, actual, ALL8, CAPS).operations == []


def test_switch_name_from_unifi_hostname():
    text = "switch.vlan.1.id=1\nswitch.vlan.1.mode=untagged\nresolv.host.1.name=SwitchKellerTest"
    desired = parse_switch_config(text, 8).config
    assert desired.switch_name == "SwitchKellerTest"
    assert plan(desired, factory_default(8), ALL8, CAPS).operations == [
        SetSwitchName("SwitchKellerTest")
    ]
    actual = factory_default(8)
    actual.switch_name = "SwitchKellerTest"
    assert plan(desired, actual, ALL8, CAPS).operations == []


def test_switch_name_is_clipped_to_what_the_switch_accepts():
    desired = factory_default(8)
    desired.switch_name = "AVeryLongUniFiDeviceNameIndeed\u00fc"
    assert plan(desired, factory_default(8), ALL8, CAPS).operations == [
        SetSwitchName("AVeryLongUniFiDevice")
    ]


def test_no_hostname_leaves_switch_name_alone():
    actual = factory_default(8)
    actual.switch_name = "manual"
    assert plan(factory_default(8), actual, ALL8, CAPS).operations == []


def test_switch_section_keeps_hostname_only():
    cfg = (
        "users.1.password=x\nresolv.host.1.name=Sw\n"
        "resolv.nameserver.1.status=disabled\nswitch.mtu=9216"
    )
    assert switch_section(cfg) == "resolv.host.1.name=Sw\nswitch.mtu=9216"
