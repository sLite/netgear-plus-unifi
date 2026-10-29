from __future__ import annotations

from pathlib import Path
from typing import ClassVar

import pytest
from py_netgear_plus import models

from netgear_collector.adapters.netgear_plus import Page

FIXTURES = Path(__file__).parent / "fixtures"


def fixture_bytes(model_dir: str, name: str) -> bytes:
    return (FIXTURES / model_dir / f"{name}.html").read_bytes()


class FakeSession:
    """Stands in for SwitchSession, serving recorded real pages.

    Uses the real py-netgear-plus model class so templates and parsers are
    exactly what production uses.
    """

    PAGES: ClassVar[dict[str, dict[Page, str]]] = {
        "GS308Ev4": {
            Page.SWITCH_INFO: "info",
            Page.PORT_STATUS: "info",
            Page.PORT_STATISTICS: "stats",
        },
        "GS116Ev2": {
            Page.SWITCH_INFO: "info",
            Page.PORT_STATUS: "status",
            Page.PORT_STATISTICS: "stats",
        },
    }

    def __init__(self, model_name: str, overrides: dict[Page, bytes] | None = None) -> None:
        self.model_class = getattr(models, model_name)
        self.model = None
        self.overrides = overrides or {}
        self.fetches: list[Page] = []
        self.logins = 0
        self.logouts = 0
        self.client_hash: str | None = None
        self.login_error: Exception | None = None
        self.fetch_error: Exception | None = None

    def detect_model(self):
        self.model = self.model_class
        return self.model

    def login(self) -> None:
        self.logins += 1
        if self.login_error:
            raise self.login_error

    def fetch(self, page: Page) -> bytes:
        self.fetches.append(page)
        if self.fetch_error:
            raise self.fetch_error
        if page in self.overrides:
            return self.overrides[page]
        name = self.model_class.MODEL_NAME
        return fixture_bytes(name.lower(), self.PAGES[name][page])

    def same_templates(self, a: Page, b: Page) -> bool:
        return getattr(self.model, a.value) == getattr(self.model, b.value)

    def set_client_hash(self, value: str | None) -> None:
        self.client_hash = value

    def logout(self) -> None:
        self.logouts += 1


class Clock:
    def __init__(self, start: float = 1000.0) -> None:
        self.now = start

    def __call__(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds


@pytest.fixture
def clock() -> Clock:
    return Clock()
