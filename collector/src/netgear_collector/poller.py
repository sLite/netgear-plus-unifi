"""Per-switch polling loop and the shared status store."""

from __future__ import annotations

import logging
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from .adapters.base import (
    AdapterError,
    AuthenticationError,
    IdentityMismatchError,
    SessionDisplacedError,
    SessionLimitError,
    SwitchAdapter,
)
from .model import CollectorState, SwitchSnapshot
from .provisioning.sync import ConfigSync
from .telemetry import RateCalculator

_LOGGER = logging.getLogger(__name__)

MAX_BACKOFF_SECONDS = 300.0
DEFAULT_SESSION_YIELD_SECONDS = 300.0
# Failures that will not fix themselves quickly; retrying fast could lock the
# switch's login or fight with an administrator's browser session.
SLOW_RETRY_ERRORS = (AuthenticationError, SessionLimitError, IdentityMismatchError)
# Repeated failures are logged at WARNING this often, DEBUG otherwise.
FAILURE_LOG_EVERY = 10


@dataclass(slots=True)
class _Entry:
    snapshot: SwitchSnapshot | None = None
    last_error: str | None = None
    consecutive_failures: int = 0


class StatusStore:
    """Latest snapshot and health per switch, shared by pollers and the API."""

    def __init__(
        self,
        switch_ids: list[str],
        stale_after_seconds: float,
        monotonic: Callable[[], float] = time.monotonic,
        syncs: dict[str, ConfigSync] | None = None,
    ) -> None:
        self._entries = {sid: _Entry() for sid in switch_ids}
        self.syncs = syncs or {}
        self._stale_after = stale_after_seconds
        self._monotonic = monotonic
        self._lock = threading.Lock()

    def record_success(self, switch_id: str, snapshot: SwitchSnapshot) -> None:
        with self._lock:
            entry = self._entries[switch_id]
            entry.snapshot = snapshot
            entry.last_error = None
            entry.consecutive_failures = 0

    def record_failure(self, switch_id: str, error: str) -> None:
        with self._lock:
            entry = self._entries[switch_id]
            entry.last_error = error
            entry.consecutive_failures += 1

    def statuses(self) -> list[dict[str, Any]]:
        with self._lock:
            return [self._render(sid, e) for sid, e in self._entries.items()]

    def _render(self, switch_id: str, entry: _Entry) -> dict[str, Any]:
        age = None
        if entry.snapshot is not None:
            age = round(self._monotonic() - entry.snapshot.monotonic_time, 3)
        if age is not None and age <= self._stale_after:
            state = CollectorState.OK
        elif entry.snapshot is None and entry.consecutive_failures == 0:
            state = CollectorState.STARTING
        else:
            state = CollectorState.UNAVAILABLE
        sync = self.syncs.get(switch_id)
        return {
            "id": switch_id,
            "state": state.value,
            "age_seconds": age,
            "last_error": entry.last_error,
            "consecutive_failures": entry.consecutive_failures,
            "snapshot": entry.snapshot.to_json() if entry.snapshot else None,
            "config_sync": sync.status() if sync else None,
        }


class SwitchPoller:
    """Polls one switch until stopped. Runs in its own thread."""

    def __init__(
        self,
        switch_id: str,
        adapter: SwitchAdapter,
        store: StatusStore,
        interval_seconds: float,
        session_yield_seconds: float = DEFAULT_SESSION_YIELD_SECONDS,
    ) -> None:
        self.switch_id = switch_id
        self._adapter = adapter
        self._store = store
        self._interval = interval_seconds
        self._session_yield = session_yield_seconds
        self._rates = RateCalculator(switch_id)
        self._failures = 0
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, name=f"poll-{switch_id}", daemon=True)

    def start(self) -> None:
        self._thread.start()

    def stop(self, timeout: float = 10.0) -> None:
        self._stop.set()
        self._thread.join(timeout)

    def _run(self) -> None:
        _LOGGER.info("polling switch=%s every %.0fs", self.switch_id, self._interval)
        try:
            while not self._stop.is_set():
                self._stop.wait(self.poll_once())
        finally:
            self._adapter.close()
            _LOGGER.info("stopped polling switch=%s", self.switch_id)

    def poll_once(self) -> float:
        """Poll once and return the delay before the next poll."""
        started = time.monotonic()
        try:
            reading = self._adapter.read()
        except AdapterError as err:
            return self._on_failure(err)
        except Exception as err:  # an adapter bug must not kill the poller
            _LOGGER.exception("unexpected adapter error switch=%s", self.switch_id)
            return self._on_failure(AdapterError(f"internal error: {type(err).__name__}"))
        snapshot = self._rates.update(reading)
        self._store.record_success(self.switch_id, snapshot)
        self._sync_config()
        if self._failures:
            _LOGGER.info(
                "switch=%s reachable again after %d failed polls", self.switch_id, self._failures
            )
        self._failures = 0
        up = sum(1 for p in snapshot.ports if p.link_up)
        _LOGGER.debug(
            "polled switch=%s ports_up=%d/%d in %.2fs",
            self.switch_id,
            up,
            len(snapshot.ports),
            time.monotonic() - started,
        )
        return self._interval

    def _sync_config(self) -> None:
        """Apply pending UniFi configuration between polls, on the same session."""
        sync = self._store.syncs.get(self.switch_id)
        factory = getattr(self._adapter, "config_writer", None)
        if sync is None or factory is None:
            return
        sync.tick(factory)

    def _on_failure(self, err: AdapterError) -> float:
        self._failures += 1
        message = f"{type(err).__name__}: {err}"
        self._store.record_failure(self.switch_id, message)
        if isinstance(err, SessionDisplacedError):
            _LOGGER.info(
                "switch=%s session displaced, probably by a web UI login; yielding for %.0fs",
                self.switch_id,
                self._session_yield,
            )
            return self._session_yield
        if isinstance(err, SLOW_RETRY_ERRORS):
            delay = MAX_BACKOFF_SECONDS
        else:
            delay = min(self._interval * 2 ** (self._failures - 1), MAX_BACKOFF_SECONDS)
        level = logging.WARNING if self._failures % FAILURE_LOG_EVERY == 1 else logging.DEBUG
        _LOGGER.log(
            level,
            "poll failed switch=%s attempt=%d retry_in=%.0fs error=%s",
            self.switch_id,
            self._failures,
            delay,
            message,
        )
        return delay
