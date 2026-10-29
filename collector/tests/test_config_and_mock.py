from __future__ import annotations

from pathlib import Path

import pytest

from netgear_collector.__main__ import build_adapter
from netgear_collector.adapters.base import AdapterError
from netgear_collector.adapters.mock import MockAdapter, MockOptions
from netgear_collector.adapters.netgear_plus import NetgearPlusAdapter
from netgear_collector.config import ConfigError, load_config, parse_config, read_password

EXAMPLE = Path(__file__).parents[2] / "config" / "switches.example.yaml"


def test_example_config_parses():
    cfg = load_config(EXAMPLE)
    assert [s.id for s in cfg.switches] == ["gs308ev4", "gs116ev2"]
    gs308 = cfg.switches[0].source
    assert (gs308.host, gs308.expected_model, gs308.poll_interval_seconds) == (
        "192.168.1.247", "GS308Ev4", 15,
    )  # fmt: skip
    assert cfg.switches[1].source.password_env == "NETGEAR_GS116EV2_PASSWORD"
    assert cfg.stale_after_seconds == 90


def test_disabled_switches_are_skipped():
    cfg = parse_config(
        {
            "switches": [
                {"id": "a", "disabled": True, "source": {"adapter": "mock"}},
                {"id": "b", "source": {"adapter": "mock", "mock": {"model": "GS116Ev2"}}},
            ]
        }
    )
    assert [s.id for s in cfg.switches] == ["b"]


@pytest.mark.parametrize(
    ("raw", "message"),
    [
        ({"switches": []}, "no enabled switches"),
        ({"switches": [{"id": "Bad_ID"}]}, "lowercase"),
        ({"switches": [{"id": "a", "source": {"adapter": "snmp"}}]}, "adapter"),
        ({"switches": [{"id": "a", "source": {"host": "h"}}]}, "password_env"),
        ({"switches": [{"id": "a", "source": {"adapter": "mock"}}] * 2}, "duplicated"),
        ({"collector": {"listen": "nope"}, "switches": [{"id": "a"}]}, "HOST:PORT"),
    ],
)
def test_invalid_config(raw, message):
    with pytest.raises(ConfigError, match=message):
        parse_config(raw)


def test_password_comes_from_environment_only():
    cfg = load_config(EXAMPLE)
    src = cfg.switches[0].source
    with pytest.raises(ConfigError, match="NETGEAR_GS308EV4_PASSWORD"):
        read_password(src, {})
    assert read_password(src, {"NETGEAR_GS308EV4_PASSWORD": "pw"}) == "pw"
    assert isinstance(
        build_adapter(cfg.switches[0], {"NETGEAR_GS308EV4_PASSWORD": "pw"}), NetgearPlusAdapter
    )


def test_mock_adapter_is_deterministic_and_counts_up(clock):
    a = MockAdapter("m", MockOptions(model="GS116Ev2", seed=3), monotonic=clock, wall=clock)
    b = MockAdapter("m", MockOptions(model="GS116Ev2", seed=3), monotonic=clock, wall=clock)
    first = a.read()
    assert first == b.read()
    assert len(first.ports) == 16
    clock.advance(10)
    later = a.read()
    for before, after in zip(first.ports, later.ports, strict=True):
        assert after.rx_bytes >= before.rx_bytes
        assert after.link_up == before.link_up


def test_mock_flapping_port_and_failures(clock):
    adapter = MockAdapter(
        "m",
        MockOptions(model="GS308Ev4", flap_port=2, flap_period_seconds=20),
        monotonic=clock,
    )
    before = adapter.read().ports[1]
    assert before.link_up
    clock.advance(10)
    down = adapter.read().ports[1]
    assert not down.link_up
    assert down.rx_bytes >= before.rx_bytes  # totals survive link loss
    failing = MockAdapter("m", MockOptions(failure_rate=1.0), monotonic=clock)
    with pytest.raises(AdapterError):
        failing.read()


def test_write_policy_is_opt_in_and_whole_switch():
    base = {"id": "a", "source": {"host": "h", "password_env": "P"}}
    assert not parse_config({"switches": [base]}).switches[0].source.write.enabled
    on = {**base, "source": {**base["source"], "write": {"enabled": True}}}
    policy = parse_config({"switches": [on]}).switches[0].source.write
    assert policy.enabled and policy.dry_run
    # A leftover port restriction must not be silently ignored.
    old = {**base, "source": {**base["source"], "write": {"enabled": True, "ports": [4]}}}
    with pytest.raises(ConfigError, match="no longer supported"):
        parse_config({"switches": [old]})
