from __future__ import annotations

import json

import pytest

from netgear_collector.adapters.mock import MockAdapter, MockOptions
from netgear_collector.provisioning.model import Member, Speed
from netgear_collector.provisioning.sync import ConfigSync, WritePolicy

U, T, E = Member.UNTAGGED, Member.TAGGED, Member.EXCLUDED

# UniFi: networks Default (1) and VLAN 3901; port 4 is an access port in
# 3901, ports without overrides use UniFi's "allow all" defaults.
ACCESS_PORT_CFG = """\
users.1.name=admin
switch.managementvlan=1
switch.vlan.1.id=1
switch.vlan.1.mode=untagged
switch.vlan.2.id=3901
switch.vlan.2.mode=tagged
switch.port.4.pvid=3901
switch.vlan.1.port.4.mode=exclude
switch.vlan.2.port.4.mode=untagged
switch.port.5.status=disabled
"""
DEFAULT_CFG = "switch.vlan.1.id=1\nswitch.vlan.1.mode=untagged\n"


@pytest.fixture
def mock(clock):
    return MockAdapter("sw", MockOptions(model="GS308Ev4"), monotonic=clock, wall=clock)


def make_sync(clock, tmp_path, **policy) -> ConfigSync:
    defaults = {"enabled": True, "dry_run": False, "debounce_seconds": 5}
    return ConfigSync(
        "sw", WritePolicy(**(defaults | policy)), tmp_path / "sw.json", monotonic=clock
    )


def run(sync: ConfigSync, mock: MockAdapter) -> None:
    sync.tick(mock.config_writer)


def test_disabled_policy_only_acknowledges(clock, tmp_path, mock):
    sync = ConfigSync("sw", WritePolicy(), None, monotonic=clock)
    sync.submit("v1", ACCESS_PORT_CFG)
    assert sync.status()["processed_cfgversion"] == "v1"
    assert sync.status()["state"] == "disabled"
    run(sync, mock)
    assert mock.config_writer().read_config().pvids[4] == 1


def test_dry_run_plans_but_does_not_write(clock, tmp_path, mock):
    sync = make_sync(clock, tmp_path, dry_run=True)
    sync.submit("v1", ACCESS_PORT_CFG)
    run(sync, mock)
    assert sync.status()["state"] == "pending"  # debounce
    clock.advance(6)
    run(sync, mock)
    status = sync.status()
    assert status["state"] == "dry_run"
    assert status["processed_cfgversion"] == "v1"
    assert status["last_plan"] == [
        "create VLAN 3901 with port 1=T, port 2=T, port 3=T, port 4=U, port 5=T, port 6=T, "
        "port 7=T, port 8=T",
        "port 4: PVID 3901",
        "VLAN 1: set port 4=E",
        "port 5: speed disabled, name ''",
    ]
    assert mock.config_writer().read_config().pvids[4] == 1


def test_live_apply_manages_the_whole_switch_and_persists(clock, tmp_path, mock):
    sync = make_sync(clock, tmp_path)
    sync.submit("v1", ACCESS_PORT_CFG)
    clock.advance(6)
    run(sync, mock)
    status = sync.status()
    assert status["state"] == "in_sync", status
    cfg = mock.config_writer().read_config()
    assert cfg.vlans[3901] == {**dict.fromkeys((1, 2, 3, 5, 6, 7, 8), T), 4: U}
    assert 4 not in cfg.vlans[1] and len(cfg.vlans[1]) == 7
    assert cfg.pvids[4] == 3901
    assert cfg.ports[5].speed is Speed.DISABLED
    assert json.loads((tmp_path / "sw.json").read_text())["processed_cfgversion"] == "v1"

    # Restart: state reloads, verification re-runs and finds nothing to do.
    restarted = make_sync(clock, tmp_path)
    assert restarted.status()["state"] == "pending"
    run(restarted, mock)
    assert restarted.status()["state"] == "in_sync"
    assert restarted.status()["last_plan"] == []


def test_reverting_in_unifi_restores_defaults(clock, tmp_path, mock):
    sync = make_sync(clock, tmp_path)
    sync.submit("v1", ACCESS_PORT_CFG)
    clock.advance(6)
    run(sync, mock)
    sync.submit("v2", DEFAULT_CFG)
    clock.advance(6)
    run(sync, mock)
    cfg = mock.config_writer().read_config()
    assert cfg.vlans == {1: dict.fromkeys(range(1, 9), U)}
    assert set(cfg.pvids.values()) == {1}
    assert cfg.ports[5].speed is Speed.AUTO


def test_drift_on_managed_port_is_corrected(clock, tmp_path, mock):
    sync = make_sync(clock, tmp_path)
    sync.submit("v1", ACCESS_PORT_CFG)
    clock.advance(6)
    run(sync, mock)
    mock._config.pvids[4] = 1  # someone changed it in the NETGEAR web UI
    clock.advance(100)
    run(sync, mock)
    assert mock._config.pvids[4] == 1  # recheck interval not reached
    clock.advance(300)
    run(sync, mock)
    assert mock._config.pvids[4] == 3901


class BrokenWriter:
    caps = None
    port_count = 8

    def read_config(self):
        raise RuntimeError("switch unreachable")


def test_failures_retry_then_give_up(clock, tmp_path):
    sync = make_sync(clock, tmp_path)
    sync.submit("v1", ACCESS_PORT_CFG)
    clock.advance(6)
    for _ in range(3):
        sync.tick(BrokenWriter)
        clock.advance(200)
    status = sync.status()
    assert status["state"] == "failed"
    assert status["processed_cfgversion"] == "v1"  # acknowledged so UniFi stops re-sending
    assert "switch unreachable" in status["last_error"]


def test_unsupported_model(clock, tmp_path):
    sync = make_sync(clock, tmp_path)
    sync.submit("v1", ACCESS_PORT_CFG)
    clock.advance(6)
    sync.tick(lambda: None)
    assert sync.status()["state"] == "unsupported"
    assert sync.status()["processed_cfgversion"] == "v1"


def test_identical_resubmission_does_not_restart_debounce(clock, tmp_path, mock):
    sync = make_sync(clock, tmp_path)
    sync.submit("v1", ACCESS_PORT_CFG)
    clock.advance(4)
    sync.submit("v1", ACCESS_PORT_CFG)  # emulator re-delivers every inform
    clock.advance(2)
    run(sync, mock)
    assert sync.status()["state"] == "in_sync"
