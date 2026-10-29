"""Adapter contract between switch vendors and the collector."""

from __future__ import annotations

from typing import Protocol

from ..model import Reading


class AdapterError(Exception):
    """A poll failed; the poller retries with backoff."""


class AuthenticationError(AdapterError):
    """Credentials were rejected. Retried slowly to avoid switch lockouts."""


class SessionLimitError(AdapterError):
    """The switch refused a new login session (e.g. a browser is logged in)."""


class SessionDisplacedError(AdapterError):
    """The switch dropped our session, typically because someone logged into
    its web UI on a single-session model (GS116Ev2). Retrying immediately
    would log that person out again, so the poller yields."""


class IdentityMismatchError(AdapterError):
    """The device at the configured address is not the expected model."""


class SwitchAdapter(Protocol):
    """Read-only access to one physical switch."""

    def read(self) -> Reading:
        """Return the current state of every port. Raises AdapterError."""
        ...

    def close(self) -> None:
        """Release any session held on the switch. Must not raise."""
        ...
