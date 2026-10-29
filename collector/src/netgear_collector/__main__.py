"""netgear-collector: poll NETGEAR Plus switches read-only and serve telemetry."""

from __future__ import annotations

import argparse
import logging
import os
import signal
import sys
import threading
from collections.abc import Mapping
from pathlib import Path

from .adapters.base import SwitchAdapter
from .adapters.mock import MockAdapter
from .adapters.netgear_plus import NetgearPlusAdapter
from .config import CollectorConfig, ConfigError, SwitchConfig, load_config, read_password
from .poller import StatusStore, SwitchPoller
from .provisioning.sync import ConfigSync
from .server import make_server

_LOGGER = logging.getLogger("netgear_collector")

# py-netgear-plus logs session cookies and request parameters at DEBUG, and
# urllib3 logs request URLs. Both stay at INFO/WARNING unless explicitly
# overridden for troubleshooting with LIBRARY_DEBUG=1.
_NOISY_LOGGERS = {"py_netgear_plus": logging.INFO, "urllib3": logging.WARNING}


def setup_logging(level_name: str, library_debug: bool) -> None:
    level = getattr(logging, level_name.upper(), logging.INFO)
    logging.basicConfig(
        level=level,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        stream=sys.stderr,
    )
    for name, floor in _NOISY_LOGGERS.items():
        logging.getLogger(name).setLevel(logging.DEBUG if library_debug else max(level, floor))
    if library_debug:
        _LOGGER.warning(
            "LIBRARY_DEBUG=1: library logs may contain session cookies; do not share them"
        )


def build_adapter(switch: SwitchConfig, environ: Mapping[str, str]) -> SwitchAdapter:
    src = switch.source
    if src.adapter == "mock":
        assert src.mock is not None
        return MockAdapter(switch.id, src.mock)
    return NetgearPlusAdapter(
        switch.id,
        host=src.host or "",
        password=read_password(src, environ),
        expected_model=src.expected_model,
    )


def run(config: CollectorConfig, environ: Mapping[str, str]) -> None:
    state_dir = Path(config.state_dir)
    state_dir.mkdir(parents=True, exist_ok=True)
    syncs = {
        s.id: ConfigSync(s.id, s.source.write, state_dir / f"{s.id}.config-sync.json")
        for s in config.switches
    }
    for sync in syncs.values():
        if sync.policy.enabled:
            _LOGGER.warning(
                "switch=%s configuration writes ENABLED, UniFi manages all ports (%s)",
                sync.switch_id,
                "dry run" if sync.policy.dry_run else "live",
            )
    store = StatusStore([s.id for s in config.switches], config.stale_after_seconds, syncs=syncs)
    pollers = [
        SwitchPoller(
            s.id,
            build_adapter(s, environ),
            store,
            s.source.poll_interval_seconds,
            s.source.session_yield_seconds,
        )
        for s in config.switches
    ]
    server = make_server(config.listen_host, config.listen_port, store)
    stop = threading.Event()

    def _shutdown(signum: int, _frame: object) -> None:
        _LOGGER.info("signal %d received, shutting down", signum)
        stop.set()

    signal.signal(signal.SIGTERM, _shutdown)
    signal.signal(signal.SIGINT, _shutdown)
    for poller in pollers:
        poller.start()
    server_thread = threading.Thread(target=server.serve_forever, name="http", daemon=True)
    server_thread.start()
    _LOGGER.info(
        "collector listening on %s:%d for %d switches",
        config.listen_host,
        config.listen_port,
        len(pollers),
    )
    stop.wait()
    server.shutdown()
    for poller in pollers:
        poller.stop()  # logs out of each switch, freeing its session slot
    _LOGGER.info("collector stopped")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="netgear-collector", description=__doc__)
    parser.add_argument("--config", default=os.environ.get("CONFIG_FILE", "/config/switches.yaml"))
    args = parser.parse_args(argv)
    setup_logging(os.environ.get("LOG_LEVEL", "INFO"), os.environ.get("LIBRARY_DEBUG") == "1")
    try:
        config = load_config(args.config)
        run(config, os.environ)
    except (ConfigError, OSError) as err:
        _LOGGER.error("startup failed: %s", err)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
