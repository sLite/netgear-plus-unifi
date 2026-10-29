"""Keeps a switch in sync with UniFi's port configuration.

The emulator submits UniFi's ``system_cfg`` (switch section) with its
``cfgversion``; the poller thread calls ``tick`` after every successful
poll, so writes share the polling session and never overlap with reads.

A cfgversion counts as *processed* once it was applied and verified, planned
in dry-run mode, or given up on after repeated failures. The emulator reports
the processed cfgversion to UniFi, so UniFi only sees a configuration as
provisioned once the hardware matches (or the failure is visible in logs and
status).
"""

from __future__ import annotations

import json
import logging
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass
from enum import StrEnum
from pathlib import Path
from typing import Any, Protocol

from .model import SwitchConfig
from .planner import Capabilities, Operation, plan
from .unifi_cfg import parse_switch_config, switch_section

_LOGGER = logging.getLogger(__name__)


class Writer(Protocol):
    caps: Capabilities
    port_count: int

    def read_config(self) -> SwitchConfig: ...

    def apply(self, op: Operation) -> None: ...


@dataclass(frozen=True, slots=True)
class WritePolicy:
    enabled: bool = False
    dry_run: bool = True
    debounce_seconds: float = 10.0
    max_attempts: int = 3
    retry_seconds: float = 30.0
    recheck_seconds: float = 300.0


class SyncState(StrEnum):
    DISABLED = "disabled"  # writes off: provisioning is only acknowledged
    WAITING = "waiting"  # no configuration from UniFi yet
    PENDING = "pending"  # new configuration, debounce or retry pending
    IN_SYNC = "in_sync"
    DRY_RUN = "dry_run"  # plan computed and logged, nothing written
    FAILED = "failed"  # gave up on this cfgversion
    UNSUPPORTED = "unsupported"  # no write protocol for this model


class ConfigSync:
    def __init__(
        self,
        switch_id: str,
        policy: WritePolicy,
        state_file: Path | None,
        monotonic: Callable[[], float] = time.monotonic,
    ) -> None:
        self.switch_id = switch_id
        self.policy = policy
        self._file = state_file
        self._now = monotonic
        self._lock = threading.Lock()
        self._desired_version: str | None = None
        self._desired_text = ""
        self._processed: str | None = None
        self._state = SyncState.WAITING if policy.enabled else SyncState.DISABLED
        self._submitted_at = 0.0
        self._attempts = 0
        self._next_attempt = 0.0
        self._last_check = 0.0
        self._last_plan: list[str] = []
        self._warnings: list[str] = []
        self._last_error: str | None = None
        self._load()

    # -- called from the HTTP thread ----------------------------------------

    def submit(self, cfgversion: str, system_cfg: str) -> None:
        text = switch_section(system_cfg)
        with self._lock:
            if cfgversion == self._desired_version and text == self._desired_text:
                return
            self._desired_version, self._desired_text = cfgversion, text
            self._submitted_at = self._now()
            self._attempts, self._next_attempt = 0, 0.0
            if not self.policy.enabled:
                self._processed = cfgversion
            else:
                self._state = SyncState.PENDING
            self._save()
        _LOGGER.info(
            "switch=%s received UniFi configuration cfgversion=%s", self.switch_id, cfgversion
        )

    def status(self) -> dict[str, Any]:
        with self._lock:
            return {
                "enabled": self.policy.enabled,
                "dry_run": self.policy.dry_run,
                "state": self._state.value,
                "desired_cfgversion": self._desired_version,
                "processed_cfgversion": self._processed,
                "last_plan": list(self._last_plan),
                "warnings": list(self._warnings),
                "last_error": self._last_error,
            }

    # -- called from the poller thread --------------------------------------

    def tick(self, writer_factory: Callable[[], Writer | None]) -> None:
        if not self.policy.enabled or not self._due():
            return
        with self._lock:
            version, text = self._desired_version, self._desired_text
        writer = writer_factory()
        if writer is None:
            self._finish(version, SyncState.UNSUPPORTED, [], [], "no write support for this model")
            return
        try:
            self._sync(writer, version, text)
        except Exception as err:  # any failure is recorded and retried
            self._on_error(version, err)

    def _due(self) -> bool:
        now = self._now()
        with self._lock:
            if self._desired_version is None:
                return False
            if self._processed != self._desired_version:
                return (
                    now - self._submitted_at >= self.policy.debounce_seconds
                    and now >= self._next_attempt
                )
            # In sync: re-check periodically to catch changes made in the
            # NETGEAR web UI (drift) on the managed ports.
            return (
                self._state is SyncState.IN_SYNC
                and now - self._last_check >= self.policy.recheck_seconds
            )

    def _sync(self, writer: Writer, version: str | None, text: str) -> None:
        desired = parse_switch_config(text, writer.port_count)
        ports = range(1, writer.port_count + 1)
        actual = writer.read_config()
        with self._lock:
            self._last_check = self._now()
        result = plan(desired.config, actual, ports, writer.caps)
        warnings = desired.warnings + result.warnings
        steps = result.describe()
        if not result.operations:
            self._finish(version, SyncState.IN_SYNC, steps, warnings, None)
            return
        if self.policy.dry_run:
            _LOGGER.info("switch=%s dry run, would apply: %s", self.switch_id, "; ".join(steps))
            self._finish(version, SyncState.DRY_RUN, steps, warnings, None)
            return
        if self._processed == version:
            _LOGGER.warning(
                "switch=%s drifted from UniFi configuration, re-applying", self.switch_id
            )
        for op in result.operations:
            writer.apply(op)
        remaining = plan(desired.config, writer.read_config(), ports, writer.caps)
        if remaining.operations:
            raise RuntimeError(f"not converged: {'; '.join(remaining.describe())}")
        _LOGGER.info(
            "switch=%s applied cfgversion=%s: %s", self.switch_id, version, "; ".join(steps)
        )
        self._finish(version, SyncState.IN_SYNC, steps, warnings, None)

    def _finish(
        self,
        version: str | None,
        state: SyncState,
        steps: list[str],
        warnings: list[str],
        error: str | None,
    ) -> None:
        with self._lock:
            if version != self._desired_version:
                return  # superseded while we worked; the next tick handles it
            changed = self._processed != version or self._state is not state
            self._processed, self._state = version, state
            self._last_plan, self._warnings, self._last_error = steps, warnings, error
            self._attempts = 0
            self._save()
        for warning in warnings if changed else []:
            _LOGGER.warning("switch=%s %s", self.switch_id, warning)

    def _on_error(self, version: str | None, err: Exception) -> None:
        message = f"{type(err).__name__}: {err}"
        with self._lock:
            self._attempts += 1
            attempts = self._attempts
            self._last_error = message
            self._next_attempt = self._now() + self.policy.retry_seconds * attempts
        if attempts < self.policy.max_attempts:
            _LOGGER.warning(
                "switch=%s apply failed (attempt %d): %s", self.switch_id, attempts, message
            )
            return
        _LOGGER.error(
            "switch=%s giving up on cfgversion=%s after %d attempts: %s",
            self.switch_id, version, attempts, message,
        )  # fmt: skip
        self._finish(version, SyncState.FAILED, self._last_plan, self._warnings, message)

    # -- persistence --------------------------------------------------------

    def _load(self) -> None:
        if self._file is None or not self._file.exists():
            return
        data = json.loads(self._file.read_text())
        self._desired_version = data.get("desired_cfgversion")
        self._desired_text = data.get("desired_text", "")
        self._processed = data.get("processed_cfgversion")
        if self.policy.enabled and self._desired_version is not None:
            # Re-verify after a restart instead of trusting the old result.
            self._processed = None
            self._state = SyncState.PENDING

    def _save(self) -> None:
        if self._file is None:
            return
        data = {
            "desired_cfgversion": self._desired_version,
            "desired_text": self._desired_text,
            "processed_cfgversion": self._processed,
        }
        tmp = self._file.with_suffix(".tmp")
        tmp.write_text(json.dumps(data, indent=2))
        tmp.replace(self._file)
