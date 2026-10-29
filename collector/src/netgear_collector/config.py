"""Loads the collector's part of the shared switches YAML file.

Only the top-level ``collector`` section and each switch's ``id``,
``disabled`` and ``source`` (physical switch) sections are read; the
``unifi`` section is the emulator's. Passwords are never stored in the file:
``source.password_env`` names the environment variable holding it.
"""

from __future__ import annotations

import os
import re
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

from .adapters.mock import MOCK_MODELS, MockOptions
from .provisioning.sync import WritePolicy

ADAPTERS = ("netgear_plus", "mock")
_ID = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$")


class ConfigError(ValueError):
    pass


@dataclass(frozen=True, slots=True)
class SourceConfig:
    adapter: str
    poll_interval_seconds: float
    write: WritePolicy = field(default_factory=WritePolicy)
    session_yield_seconds: float = 300.0
    host: str | None = None
    password_env: str | None = None
    expected_model: str | None = None
    mock: MockOptions | None = None


@dataclass(frozen=True, slots=True)
class SwitchConfig:
    id: str
    source: SourceConfig


@dataclass(frozen=True, slots=True)
class CollectorConfig:
    listen_host: str = "0.0.0.0"
    listen_port: int = 8099
    stale_after_seconds: float = 90.0
    state_dir: str = "/data"
    switches: tuple[SwitchConfig, ...] = field(default_factory=tuple)


def load_config(path: str | Path) -> CollectorConfig:
    with Path(path).open(encoding="utf-8") as fh:
        raw = yaml.safe_load(fh) or {}
    return parse_config(raw)


def parse_config(raw: Mapping[str, Any]) -> CollectorConfig:
    section = raw.get("collector") or {}
    default_interval = _positive(
        section.get("poll_interval_seconds", 15), "collector.poll_interval_seconds"
    )
    host, _, port = str(section.get("listen", "0.0.0.0:8099")).rpartition(":")
    if not host or not port.isdigit():
        raise ConfigError("collector.listen must look like HOST:PORT")
    switches = []
    seen: set[str] = set()
    for entry in raw.get("switches") or []:
        sw = _parse_switch(entry, default_interval)
        if sw is None:
            continue
        if sw.id in seen:
            raise ConfigError(f"switch id {sw.id!r} is duplicated")
        seen.add(sw.id)
        switches.append(sw)
    if not switches:
        raise ConfigError("no enabled switches configured")
    return CollectorConfig(
        listen_host=host,
        listen_port=int(port),
        state_dir=str(section.get("state_dir", "/data")),
        stale_after_seconds=_positive(
            section.get("stale_after_seconds", 90), "collector.stale_after_seconds"
        ),
        switches=tuple(switches),
    )


def _parse_switch(entry: Mapping[str, Any], default_interval: float) -> SwitchConfig | None:
    switch_id = str(entry.get("id", ""))
    if not _ID.match(switch_id):
        raise ConfigError(f"switch id {switch_id!r}: use lowercase letters, digits and dashes")
    if entry.get("disabled"):
        return None
    src = entry.get("source") or {}
    adapter = src.get("adapter", "netgear_plus")
    if adapter not in ADAPTERS:
        raise ConfigError(f"switch {switch_id}: source.adapter must be one of {ADAPTERS}")
    interval = _positive(
        src.get("poll_interval_seconds", default_interval), f"switch {switch_id} poll interval"
    )
    if adapter == "mock":
        mock = src.get("mock") or {}
        options = MockOptions(
            model=mock.get("model", "GS308Ev4"),
            seed=int(mock.get("seed", 0)),
            failure_rate=float(mock.get("failure_rate", 0.0)),
            flap_port=mock.get("flap_port"),
            flap_period_seconds=float(mock.get("flap_period_seconds", 60)),
        )
        if options.model not in MOCK_MODELS:
            raise ConfigError(
                f"switch {switch_id}: mock.model must be one of {sorted(MOCK_MODELS)}"
            )
        return SwitchConfig(
            switch_id,
            SourceConfig(adapter, interval, write=_write_policy(switch_id, src), mock=options),
        )
    for key in ("host", "password_env"):
        if not src.get(key):
            raise ConfigError(f"switch {switch_id}: source.{key} is required")
    return SwitchConfig(
        switch_id,
        SourceConfig(
            adapter,
            interval,
            write=_write_policy(switch_id, src),
            session_yield_seconds=_positive(
                src.get("session_yield_seconds", 300), f"switch {switch_id} session yield"
            ),
            host=str(src["host"]),
            password_env=str(src["password_env"]),
            expected_model=src.get("expected_model"),
        ),
    )


def _write_policy(switch_id: str, src: Mapping[str, Any]) -> WritePolicy:
    """Writes are opt-in per switch. When enabled, UniFi manages every port."""
    raw = src.get("write") or {}
    if "ports" in raw:
        raise ConfigError(
            f"switch {switch_id}: source.write.ports is no longer supported; "
            "with writes enabled UniFi manages every port of the switch"
        )
    if not raw.get("enabled"):
        return WritePolicy()
    return WritePolicy(
        enabled=True,
        dry_run=bool(raw.get("dry_run", True)),
        debounce_seconds=_positive(raw.get("debounce_seconds", 10), f"switch {switch_id} debounce"),
    )


def _positive(value: Any, name: str) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError) as err:
        raise ConfigError(f"{name} must be a number") from err
    if number <= 0:
        raise ConfigError(f"{name} must be positive")
    return number


def read_password(source: SourceConfig, environ: Mapping[str, str] = os.environ) -> str:
    password = environ.get(source.password_env or "")
    if not password:
        raise ConfigError(f"environment variable {source.password_env} is not set")
    return password
