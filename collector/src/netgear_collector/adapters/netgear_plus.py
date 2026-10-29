"""NETGEAR Plus polling adapter built on py-netgear-plus.

The library is used for what it does well: model autodetection, the
per-model login hashing, session-cookie handling with transparent re-login,
and its per-model page parsers. Polling goes through ``SwitchSession``, which
fetches only the three status pages. The library's own write operations
(VLANs, PoE, port switching, reboot, IP and password changes) are never
called; configuration writes, when enabled, are implemented in
``netgear_write`` on the same session.

Library internals touched here (pinned commit, see pyproject.toml):
``NetgearSwitchConnector._client_hash`` must be set from the switch-info page
before the GS108Ev4-family statistics template can be requested; the
library's own ``get_switch_infos`` does the same internally.
"""

from __future__ import annotations

import contextlib
import logging
import time
from collections.abc import Callable
from enum import StrEnum
from typing import Any

import requests
from lxml import etree
from py_netgear_plus import PORT_STATUS_CONNECTED, NetgearSwitchConnector
from py_netgear_plus import parsers as ngp_parsers
from py_netgear_plus.fetcher import (
    BaseResponse,
    EmptyTemplateParameterError,
    LoginFailedError,
    MaxSessionsError,
    NotLoggedInError,
    PageFetcherConnectionError,
    PageNotLoadedError,
)
from py_netgear_plus.models import MultipleModelsDetectedError, SwitchModelNotDetectedError
from py_netgear_plus.parsers import NetgearPlusPageParserError, create_page_parser

from ..model import DeviceInfo, PortReading, Reading
from . import netgear_pages as pages
from .base import (
    AdapterError,
    AuthenticationError,
    IdentityMismatchError,
    SessionDisplacedError,
    SessionLimitError,
)
from .netgear_write import WRITERS, ConfigWriter, advertised_speeds

_LOGGER = logging.getLogger(__name__)

# Models whose page formats this project verified against real hardware.
VERIFIED_MODELS = {"GS308Ev4": "V1.0.1.9", "GS116Ev2": "2.6.0.50"}

# Errors from the library and its HTTP/HTML stack that mean "this poll failed".
_TRANSIENT_ERRORS = (
    PageNotLoadedError,
    PageFetcherConnectionError,
    NotLoggedInError,
    EmptyTemplateParameterError,
    NetgearPlusPageParserError,
    SwitchModelNotDetectedError,
    MultipleModelsDetectedError,
    requests.RequestException,
    etree.ParserError,
    ValueError,
    IndexError,
    KeyError,
    AttributeError,
)


class Page(StrEnum):
    """The only pages this project ever requests (model template attributes)."""

    SWITCH_INFO = "SWITCH_INFO_TEMPLATES"
    PORT_STATUS = "PORT_STATUS_TEMPLATES"
    PORT_STATISTICS = "PORT_STATISTICS_TEMPLATES"


class SwitchSession:
    """The single login session between this project and a NETGEAR switch."""

    def __init__(self, host: str, password: str) -> None:
        self._connector = NetgearSwitchConnector(host, password)
        self.model: Any = None

    def detect_model(self) -> Any:
        self.model = self._connector.autodetect_model()
        return self.model

    def login(self) -> None:
        if not self._connector.get_login_cookie():
            raise AuthenticationError("switch rejected the login")

    def fetch(self, page: Page) -> bytes:
        templates = getattr(self.model, page.value)
        return self._connector.fetch_page_from_templates(templates).content

    def same_templates(self, a: Page, b: Page) -> bool:
        return getattr(self.model, a.value) == getattr(self.model, b.value)

    def set_client_hash(self, value: str | None) -> None:
        self._connector._client_hash = value

    def logout(self) -> None:
        self._connector.delete_login_cookie()


SessionFactory = Callable[[str, str], SwitchSession]


def _as_response(content: bytes) -> BaseResponse:
    response = BaseResponse()
    response.status_code = 200
    response.content = content
    return response


class NetgearPlusAdapter:
    """Polls one NETGEAR Plus switch, keeping one login session open."""

    METADATA_REFRESH_SECONDS = 600

    def __init__(
        self,
        switch_id: str,
        host: str,
        password: str,
        expected_model: str | None = None,
        session_factory: SessionFactory = SwitchSession,
        monotonic: Callable[[], float] = time.monotonic,
        wall: Callable[[], float] = time.time,
    ) -> None:
        self._id = switch_id
        self._host = host
        self._password = password
        self._expected_model = expected_model
        self._session_factory = session_factory
        self._monotonic = monotonic
        self._wall = wall
        self._session: SwitchSession | None = None
        self._parser: Any = None
        self._device: DeviceInfo | None = None
        self._device_read_at = 0.0

    def read(self) -> Reading:
        try:
            if self._session is None:
                self._connect()
            return self._read_once()
        except AdapterError:
            self._drop_session()
            raise
        except MaxSessionsError as err:
            self._drop_session()
            raise SessionLimitError(str(err)) from err
        except LoginFailedError as err:
            self._drop_session()
            raise AdapterError(f"session lost and re-login failed: {err}") from err
        except _TRANSIENT_ERRORS as err:
            self._drop_session()
            raise AdapterError(f"{type(err).__name__}: {err}") from err

    def close(self) -> None:
        self._drop_session()

    def config_writer(self) -> ConfigWriter | None:
        """A writer bound to the current session, or None when the model has
        no verified write protocol or no session is open. Writes reuse the
        polling session (the GS116Ev2 allows only one)."""
        session = self._session
        if session is None:
            return None
        writer = WRITERS.get(session.model.MODEL_NAME)
        if writer is None:
            return None
        return writer(session, self._host, session.model.PORTS)

    def _connect(self) -> None:
        session = self._session_factory(self._host, self._password)
        model = session.detect_model()
        name = model.MODEL_NAME
        if self._expected_model and name != self._expected_model:
            raise IdentityMismatchError(
                f"{self._host} is a {name}, configuration expects {self._expected_model}"
            )
        try:
            session.login()
        except LoginFailedError as err:
            raise AuthenticationError(f"login rejected: {err}") from err
        self._session = session
        self._parser = create_page_parser(name)
        self._device = None
        _LOGGER.info("logged in switch=%s host=%s model=%s", self._id, self._host, name)
        if name not in VERIFIED_MODELS:
            _LOGGER.warning(
                "switch=%s model %s is supported by py-netgear-plus but not verified by this "
                "project; check port data carefully",
                self._id,
                name,
            )

    def _drop_session(self) -> None:
        session, self._session = self._session, None
        if session is None:
            return
        with contextlib.suppress(Exception):
            session.logout()
            _LOGGER.debug("logged out switch=%s", self._id)

    def _read_once(self) -> Reading:
        session = self._session
        assert session is not None
        info_page = None
        if self._device is None or self._metadata_due():
            info_page = self._fetch(Page.SWITCH_INFO)
            session.set_client_hash(self._parser.parse_client_hash(_as_response(info_page)))
            self._device = self._parse_device(info_page)
            self._device_read_at = self._monotonic()
        if info_page is not None and session.same_templates(Page.SWITCH_INFO, Page.PORT_STATUS):
            status_page = info_page
        else:
            status_page = self._fetch(Page.PORT_STATUS)
        stats_page = self._fetch(Page.PORT_STATISTICS)
        ports = self._parse_ports(status_page, stats_page)
        return Reading(
            device=self._device,
            ports=ports,
            monotonic_time=self._monotonic(),
            wall_time=self._wall(),
        )

    def _fetch(self, page: Page) -> bytes:
        content = self._session.fetch(page)
        if pages.is_login_redirect(content):
            raise SessionDisplacedError(
                "switch redirected to its login page; another login (e.g. the web UI) "
                "probably displaced this session"
            )
        return content

    def _metadata_due(self) -> bool:
        return self._monotonic() - self._device_read_at > self.METADATA_REFRESH_SECONDS

    def _parse_device(self, info_page: bytes) -> DeviceInfo:
        model = self._session.model
        meta = self._parser.parse_switch_metadata(_as_response(info_page))
        writer = WRITERS.get(model.MODEL_NAME)
        if isinstance(self._parser, ngp_parsers.GS108Ev4):
            mac = pages.gs108ev4_hardware_mac(info_page)
        elif isinstance(self._parser, ngp_parsers.JGSxxxSeries):
            mac = pages.jgs_hardware_mac(info_page)
        else:
            mac = None
        device = DeviceInfo(
            vendor="netgear",
            model=model.MODEL_NAME,
            port_count=model.PORTS,
            management_ip=self._host,
            firmware=meta.get("switch_firmware") or None,
            serial=meta.get("switch_serial_number") or None,
            hardware_mac=mac,
            port_speeds=advertised_speeds(writer.caps) if writer else (),
        )
        verified = VERIFIED_MODELS.get(device.model)
        if verified and device.firmware and device.firmware != verified:
            _LOGGER.warning(
                "switch=%s firmware %s differs from the verified %s; page formats may differ",
                self._id,
                device.firmware,
                verified,
            )
        return device

    def _parse_ports(self, status_page: bytes, stats_page: bytes) -> tuple[PortReading, ...]:
        count = self._session.model.PORTS
        status = self._parser.parse_port_status(_as_response(status_page), count)
        counters = self._parse_counters(stats_page, count)
        extras = (
            pages.gs108ev4_port_extras(status_page)
            if isinstance(self._parser, ngp_parsers.GS108Ev4)
            else {}
        )
        ports = []
        for index in range(1, count + 1):
            raw = status.get(index)
            if raw is None:
                raise AdapterError(f"port {index} missing from status page")
            ports.append(_port_reading(index, raw, counters.get(index), extras.get(index)))
        return tuple(ports)

    def _parse_counters(self, stats_page: bytes, count: int) -> dict[int, pages.PortCounters]:
        if isinstance(self._parser, ngp_parsers.GS108Ev4):
            return pages.gs108ev4_counters(stats_page)
        raw = self._parser.parse_port_statistics(_as_response(stats_page), count)
        return {
            i + 1: pages.PortCounters(raw["sum_rx"][i], raw["sum_tx"][i], raw["crc_errors"][i])
            for i in range(count)
        }


def _port_reading(
    index: int,
    raw: dict[str, Any],
    counters: pages.PortCounters | None,
    extras: pages.PortExtras | None,
) -> PortReading:
    link_up = raw.get("status") in PORT_STATUS_CONNECTED
    mode = raw.get("modus_speed")
    mode_text = mode.strip().lower() if isinstance(mode, str) else None
    speed = pages.parse_link_speed(raw.get("connection_speed")) if link_up else 0
    name = raw.get("description") or (extras.name if extras else None)
    return PortReading(
        index=index,
        link_up=link_up,
        speed_mbps=speed,
        full_duplex=extras.full_duplex if extras and link_up else None,
        autoneg=(mode_text == "auto") if mode_text else None,
        enabled=(mode_text != "disable") if mode_text else None,
        name=name,
        rx_bytes=counters.rx_bytes if counters else None,
        tx_bytes=counters.tx_bytes if counters else None,
        crc_errors=counters.crc_errors if counters else None,
    )
