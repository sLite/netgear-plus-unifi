"""Switch adapters. Add a vendor or model family by implementing SwitchAdapter."""

from .base import (
    AdapterError,
    AuthenticationError,
    IdentityMismatchError,
    SessionDisplacedError,
    SessionLimitError,
    SwitchAdapter,
)

__all__ = [
    "AdapterError",
    "AuthenticationError",
    "IdentityMismatchError",
    "SessionDisplacedError",
    "SessionLimitError",
    "SwitchAdapter",
]
