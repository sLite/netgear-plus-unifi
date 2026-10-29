from __future__ import annotations

import json
import threading
import urllib.request

import pytest

from netgear_collector.adapters.base import (
    AdapterError,
    AuthenticationError,
    SessionDisplacedError,
    SessionLimitError,
)
from netgear_collector.model import DeviceInfo, PortReading, Reading
from netgear_collector.poller import MAX_BACKOFF_SECONDS, StatusStore, SwitchPoller
from netgear_collector.server import make_server


def status_of(store, switch_id="sw"):
    return next(s for s in store.statuses() if s["id"] == switch_id)


DEVICE = DeviceInfo(vendor="netgear", model="GS308Ev4", port_count=1, management_ip="192.0.2.1")


class ScriptedAdapter:
    """Returns readings or raises errors in the given order."""

    def __init__(self, clock, script):
        self.clock = clock
        self.script = list(script)
        self.closed = False

    def read(self):
        item = self.script.pop(0)
        if isinstance(item, Exception):
            raise item
        port = PortReading(index=1, link_up=True, speed_mbps=1000, rx_bytes=item, tx_bytes=item)
        return Reading(DEVICE, (port,), monotonic_time=self.clock(), wall_time=1.7e9)

    def close(self):
        self.closed = True


def test_store_states_follow_snapshot_age(clock):
    store = StatusStore(["sw"], stale_after_seconds=90, monotonic=clock)
    assert status_of(store)["state"] == "starting"
    poller = SwitchPoller("sw", ScriptedAdapter(clock, [100]), store, 15)
    poller.poll_once()
    status = status_of(store)
    assert status["state"] == "ok"
    assert status["snapshot"]["ports"][0]["rx_bytes"] == 100
    assert "monotonic_time" not in status["snapshot"]
    clock.advance(91)
    assert status_of(store)["state"] == "unavailable"


def test_failure_before_first_success_is_unavailable(clock):
    store = StatusStore(["sw"], 90, monotonic=clock)
    SwitchPoller("sw", ScriptedAdapter(clock, [AdapterError("down")]), store, 15).poll_once()
    status = status_of(store)
    assert status["state"] == "unavailable"
    assert status["last_error"] == "AdapterError: down"
    assert status["consecutive_failures"] == 1


def test_backoff_doubles_and_recovery_resets(clock):
    store = StatusStore(["sw"], 90, monotonic=clock)
    errors = [AdapterError("x")] * 7
    poller = SwitchPoller("sw", ScriptedAdapter(clock, [*errors, 5]), store, 15)
    delays = [poller.poll_once() for _ in errors]
    assert delays == [15, 30, 60, 120, 240, 300, 300]
    assert poller.poll_once() == 15
    assert status_of(store)["consecutive_failures"] == 0


@pytest.mark.parametrize("error", [AuthenticationError("no"), SessionLimitError("busy")])
def test_slow_retry_for_auth_and_session_errors(clock, error):
    store = StatusStore(["sw"], 90, monotonic=clock)
    assert (
        SwitchPoller("sw", ScriptedAdapter(clock, [error]), store, 15).poll_once()
        == MAX_BACKOFF_SECONDS
    )


def test_unexpected_exception_does_not_kill_poller(clock):
    store = StatusStore(["sw"], 90, monotonic=clock)
    poller = SwitchPoller("sw", ScriptedAdapter(clock, [RuntimeError("bug")]), store, 15)
    assert poller.poll_once() == 15
    assert "internal error" in status_of(store)["last_error"]


def test_thread_lifecycle_closes_adapter(clock):
    store = StatusStore(["sw"], 90, monotonic=clock)
    adapter = ScriptedAdapter(clock, [1] * 100)
    poller = SwitchPoller("sw", adapter, store, 3600)
    poller.start()
    poller.stop()
    assert adapter.closed


def test_http_api(clock):
    store = StatusStore(["sw"], 90, monotonic=clock)
    SwitchPoller("sw", ScriptedAdapter(clock, [42]), store, 15).poll_once()
    server = make_server("127.0.0.1", 0, store)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{server.server_address[1]}"
    try:
        with urllib.request.urlopen(base + "/v1/switches") as resp:
            body = json.load(resp)
        assert body["switches"][0]["snapshot"]["ports"][0]["tx_bytes"] == 42
        with pytest.raises(urllib.error.HTTPError) as err:
            urllib.request.urlopen(base + "/v1/switches/sw")
        assert err.value.code == 404
    finally:
        server.shutdown()


def test_displaced_session_yields_for_configured_time(clock):
    store = StatusStore(["sw"], 90, monotonic=clock)
    adapter = ScriptedAdapter(clock, [SessionDisplacedError("web UI login")])
    assert SwitchPoller("sw", adapter, store, 15, session_yield_seconds=120).poll_once() == 120
    assert "SessionDisplacedError" in status_of(store)["last_error"]


def test_http_config_submission(clock):
    from netgear_collector.provisioning.sync import ConfigSync, WritePolicy

    sync = ConfigSync("sw", WritePolicy(), None, monotonic=clock)
    store = StatusStore(["sw"], 90, monotonic=clock, syncs={"sw": sync})
    server = make_server("127.0.0.1", 0, store)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{server.server_address[1]}"

    def post(path, body):
        req = urllib.request.Request(base + path, data=json.dumps(body).encode(), method="POST")
        return urllib.request.urlopen(req)

    try:
        with post("/v1/switches/sw/config", {"cfgversion": "abc", "system_cfg": "x=1"}) as resp:
            assert resp.status == 202
            assert json.load(resp)["processed_cfgversion"] == "abc"
        with urllib.request.urlopen(base + "/v1/switches") as resp:
            assert json.load(resp)["switches"][0]["config_sync"]["desired_cfgversion"] == "abc"
        for path, body in (("/v1/switches/nope/config", {}), ("/v1/switches/sw/config", {})):
            with pytest.raises(urllib.error.HTTPError) as err:
                post(path, body)
            assert err.value.code in (400, 404)
    finally:
        server.shutdown()
