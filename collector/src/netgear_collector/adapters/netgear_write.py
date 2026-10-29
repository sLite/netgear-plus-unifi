"""Configuration writers for NETGEAR Plus switches (opt-in, port-scoped).

The endpoints were taken from the switches' own web-UI code (see
docs/CONFIG-WRITE.md):

* GS308Ev4: the GS30x protocol also used by py-netgear-plus for GS305E/GS308EP
  (``8021qAdvanced.cgi``, ``portPVID.cgi``, ``port_status.cgi`` with the
  session ``hash``).
* GS116Ev2: JGS form posts (``config/dot1based_advanced_*.htm``,
  ``config/status_status.htm``) with a per-page ``secureRand``.

Every write re-reads the switch right before writing, merges the planned
change into that fresh state, and verifies the result by reading back. If
anything the operation did not name changed (another port's membership, a
PVID), the writer restores the expected membership and raises. VLAN 1 and the
switch's management VLAN are never deleted.

Never implemented here, deliberately: VLAN mode changes (``vlanMod.cgi``,
``changeType``), management VLAN, IP settings, password, reboot, reset,
firmware.
"""

from __future__ import annotations

import logging
import re
import time
from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import Any

from lxml import html
from py_netgear_plus.fetcher import BaseResponse
from py_netgear_plus.parsers import create_page_parser

from ..provisioning.model import Member, PortSettings, Speed, SwitchConfig
from ..provisioning.planner import (
    Capabilities,
    CreateVlan,
    DeleteVlan,
    Operation,
    SetMembers,
    SetPort,
    SetPvid,
    SetSwitchName,
    protected_vlans,
    switch_name,
)
from . import netgear_pages as pages
from .base import AdapterError, SessionDisplacedError

_LOGGER = logging.getLogger(__name__)

MAX_VID = 4093  # both UIs reject higher ids for user VLANs


class WriteError(AdapterError):
    """A configuration write failed or could not be verified."""


class ForbiddenWriteError(WriteError):
    """An operation would touch something outside the write policy."""


class ConfigWriter(ABC):
    """Reads and changes VLAN/port configuration of one switch."""

    caps: Capabilities

    def __init__(self, session: Any, host: str, port_count: int):
        self._session = session
        self._host = host
        self.port_count = port_count

    # -- public API ---------------------------------------------------------

    @abstractmethod
    def read_config(self) -> SwitchConfig: ...

    def apply(self, op: Operation) -> None:
        """Apply one planned operation and verify it by reading back."""
        before = self.read_config()
        self._guard(op, before)
        _LOGGER.info("write host=%s: %s", self._host, op)
        match op:
            case CreateVlan(vid, members):
                self._create_vlan(vid, members, before)
                self._set_members(vid, members, self.read_config())
            case SetMembers(vid, members):
                self._set_members(vid, members, before)
            case SetPvid(port, vid):
                self._set_pvid(port, vid)
            case DeleteVlan(vid):
                self._delete_vlan(vid)
            case SetPort(port, settings):
                self._set_port(port, settings, before)
            case SetSwitchName(name):
                self._set_switch_name(name)
        self._verify(op, before)

    # -- model specific -----------------------------------------------------

    @abstractmethod
    def _create_vlan(self, vid: int, members: dict[int, Member], before: SwitchConfig) -> None: ...

    @abstractmethod
    def _write_members(self, vid: int, members: dict[int, Member], before: SwitchConfig) -> None:
        """Write the complete membership of ``vid`` (all ports)."""

    @abstractmethod
    def _set_pvid(self, port: int, vid: int) -> None: ...

    @abstractmethod
    def _delete_vlan(self, vid: int) -> None: ...

    @abstractmethod
    def _set_port(self, port: int, settings: PortSettings, before: SwitchConfig) -> None: ...

    @abstractmethod
    def _set_switch_name(self, name: str) -> None: ...

    # -- shared logic -------------------------------------------------------

    def _guard(self, op: Operation, before: SwitchConfig) -> None:
        ports: set[int] = set()
        match op:
            case CreateVlan(vid, members) | SetMembers(vid, members):
                ports = set(members)
                if not 1 <= vid <= MAX_VID:
                    raise ForbiddenWriteError(f"VLAN id {vid} out of range")
            case SetPvid(port, _) | SetPort(port, _):
                ports = {port}
            case DeleteVlan(vid) if vid in protected_vlans(before):
                raise ForbiddenWriteError(f"VLAN {vid} (default/management) is never deleted")
            case SetSwitchName(name) if name != switch_name(name):
                raise ForbiddenWriteError(f"switch name {name!r} is not valid for the switch")
        invalid = {p for p in ports if not 1 <= p <= self.port_count}
        if invalid:
            raise ForbiddenWriteError(f"ports {sorted(invalid)} do not exist on this switch")

    def _set_members(self, vid: int, changes: dict[int, Member], before: SwitchConfig) -> None:
        merged = _merge(before.vlans.get(vid, {}), changes)
        if merged == before.vlans.get(vid, {}):
            return
        if not merged:
            raise ForbiddenWriteError(f"VLAN {vid} would have no members")
        self._write_members(vid, merged, before)

    def _verify(self, op: Operation, before: SwitchConfig) -> None:
        after = self.read_config()
        self._check_side_effects(op, before, after)
        problem = _verify_op(op, after)
        if problem:
            raise WriteError(f"{op}: not applied ({problem})")

    def _check_side_effects(self, op: Operation, before: SwitchConfig, after: SwitchConfig) -> None:
        """Last line of defence: nothing the operation did not name may change.

        A VLAN whose membership differs from what the operation should have
        produced is written back to that expected membership immediately.
        """
        for vid, expected in _expected_vlans(op, before).items():
            actual = after.vlans.get(vid)
            if actual is None or actual == expected:
                continue
            _LOGGER.error(
                "host=%s VLAN %d: unexpected membership %s (expected %s); restoring",
                self._host, vid, actual, expected,
            )  # fmt: skip
            if expected:
                self._write_members(vid, expected, after)
            raise WriteError(f"VLAN {vid}: unexpected change on other ports, restored")
        changed_pvid = op.port if isinstance(op, SetPvid) else None
        if any(after.pvids.get(p) != v for p, v in before.pvids.items() if p != changed_pvid):
            raise WriteError("PVID of a port the operation did not name changed")

    # -- HTTP helpers -------------------------------------------------------

    def _get(self, path: str) -> bytes:
        content = self._request("get", path, None)
        if pages.is_login_redirect(content):
            raise SessionDisplacedError("session lost while reading configuration")
        return content

    def _post(self, path: str, data: list[tuple[str, str]]) -> bytes:
        return self._request("post", path, data)

    def _request(self, method: str, path: str, data: list[tuple[str, str]] | None) -> bytes:
        fetcher = self._session._connector._page_fetcher
        response = fetcher.request(method, f"http://{self._host}/{path}", data)
        if not fetcher.has_ok_status(response):
            raise WriteError(f"{method.upper()} {path}: HTTP {response.status_code}")
        return response.content or b""


def _merge(current: dict[int, Member], changes: dict[int, Member]) -> dict[int, Member]:
    merged = dict(current)
    for port, member in changes.items():
        if member is Member.EXCLUDED:
            merged.pop(port, None)
        else:
            merged[port] = member
    return merged


def _expected_vlans(op: Operation, before: SwitchConfig) -> dict[int, dict[int, Member]]:
    """Every VLAN's membership as the operation should leave it."""
    expected = {vid: dict(members) for vid, members in before.vlans.items()}
    match op:
        case CreateVlan(vid, members) | SetMembers(vid, members):
            expected[vid] = _merge(expected.get(vid, {}), members)
        case DeleteVlan(vid):
            expected.pop(vid, None)
    return expected


def _verify_op(op: Operation, after: SwitchConfig) -> str | None:
    match op:
        case CreateVlan(vid, members) | SetMembers(vid, members):
            for port, member in members.items():
                if after.member(vid, port) is not member:
                    return f"port {port} is {after.member(vid, port).value}"
        case SetPvid(port, vid) if after.pvids.get(port) != vid:
            return f"PVID is {after.pvids.get(port)}"
        case DeleteVlan(vid) if vid in after.vlans:
            return "VLAN still exists"
        case SetSwitchName(name) if (after.switch_name or "") != name:
            return f"name is {after.switch_name!r}"
        case SetPort(port, settings):
            have = after.ports.get(port, PortSettings())
            if have.speed is not settings.speed:
                return f"speed is {have.speed.value}"
            if settings.name is not None and (have.name or "") != settings.name:
                return f"name is {have.name!r}"
    return None


# ---------------------------------------------------------------------------
# GS308Ev4 (GS30x protocol)
# ---------------------------------------------------------------------------

_GS30X_MEMBER_CODE = {Member.TAGGED: "1", Member.UNTAGGED: "2", Member.EXCLUDED: "3"}
_GS30X_SPEED_CODE = {
    Speed.AUTO: "1",
    Speed.DISABLED: "2",
    Speed.M10_FULL: "4",
    Speed.M100_FULL: "6",
}
_GS30X_SPEED_FROM_CODE = {
    "1": Speed.AUTO,
    "2": Speed.DISABLED,
    "3": Speed.M10_HALF,
    "4": Speed.M10_FULL,
    "5": Speed.M100_HALF,
    "6": Speed.M100_FULL,
}
# The port settings apply restarts the port; the web UI waits as well.
_GS30X_PORT_SETTLE_SECONDS = 3.0


class Gs30xWriter(ConfigWriter):
    """GS308Ev4. Its web UI uses the GS30x protocol."""

    caps = Capabilities(
        speeds=frozenset({Speed.AUTO, Speed.DISABLED, Speed.M10_FULL, Speed.M100_FULL}),
        max_vlans=64,
    )

    def read_config(self) -> SwitchConfig:
        vlan_page = self._get("vlan.cgi")
        self._vlan_form = _gs30x_vlan_form(vlan_page)
        raw = create_page_parser("GS308EP").parse_vlan_status(_response(vlan_page))
        if raw.get("mode") != "adv8021Q":
            raise WriteError(f"VLAN mode is {raw.get('mode')}, advanced 802.1Q required")
        cfg = SwitchConfig()
        self._vlan_names: dict[int, str] = {}
        for vid, info in raw["vlans"].items():
            members = {int(p): Member(getattr(m, "value", m)) for p, m in info["cfg"].items()}
            cfg.vlans[int(vid)] = {p: m for p, m in members.items() if m is not Member.EXCLUDED}
            self._vlan_names[int(vid)] = info.get("name") or ""
        cfg.pvids = {int(p): int(info["pvid"]) for p, info in raw["ports"].items()}
        mgmt = self._vlan_form.get("mgmtVlan") or "1"
        cfg.management_vlan = int(mgmt) if mgmt.isdigit() else 1
        dashboard = self._get("dashboard.cgi")
        self._dashboard = _gs30x_port_forms(dashboard)
        self._system_form = _gs30x_system_form(dashboard)
        cfg.switch_name = self._system_form["switch_name"] or None
        for port, form in self._dashboard.items():
            cfg.ports[port] = PortSettings(
                speed=_GS30X_SPEED_FROM_CODE.get(form.get("Speed", ""), Speed.AUTO),
                name=form.get("portName") or None,
            )
        return cfg

    def _hidden_mem(self, members: dict[int, Member]) -> str:
        return "".join(
            _GS30X_MEMBER_CODE[members.get(p, Member.EXCLUDED)]
            for p in range(1, self.port_count + 1)
        )

    def _vlan_post(self, fields: list[tuple[str, str]]) -> None:
        content = self._post("8021qAdvanced.cgi", [("hash", self._vlan_form["hash"]), *fields])
        _gs30x_check(content, ok_prefix=b"<div")

    def _create_vlan(self, vid: int, members: dict[int, Member], before: SwitchConfig) -> None:
        self._vlan_post([
            ("ACTION", "Add"),
            ("VLAN_ID", str(vid)),
            ("vlanNum", self._vlan_form["vlanNum"]),
            ("VLAN_NAME", f"unifi-{vid}"),
            ("hiddenMem", self._hidden_mem(members)),
            ("voiceVLANID", "0"),
            ("voiceVlanCos", self._vlan_form["cos"]),
        ])  # fmt: skip

    def _write_members(self, vid: int, members: dict[int, Member], before: SwitchConfig) -> None:
        voice = "1" if self._vlan_form["voVid"] == str(vid) else "0"
        self._vlan_post([
            ("ACTION", "Apply"),
            ("VLAN_ID", str(vid)),
            ("vlanNum", self._vlan_form["vlanNum"]),
            ("VLAN_NAME", self._vlan_names.get(vid, "")),
            ("hiddenMem", self._hidden_mem(members)),
            ("voiceVLANID", voice),
            ("voiceVlanCos", self._vlan_form["cos"]),
        ])  # fmt: skip

    def _delete_vlan(self, vid: int) -> None:
        self._vlan_post([("ACTION", "Delete"), ("VLAN_ID", str(vid))])

    def _set_pvid(self, port: int, vid: int) -> None:
        content = self._post(
            "portPVID.cgi",
            [("hash", self._vlan_form["hash"]), ("PORT", str(port)), ("PVID", str(vid))],
        )
        _gs30x_check(content, ok_prefix=b"<div")

    def _set_port(self, port: int, settings: PortSettings, before: SwitchConfig) -> None:
        form = self._dashboard[port]
        content = self._post("port_status.cgi", [
            ("hash", self._vlan_form["hash"]),
            (f"port{port}", "checked"),
            ("SPEED", _GS30X_SPEED_CODE[settings.speed]),
            ("FLOW_CONTROL", form.get("flowCtr", "2")),
            ("DESCRIPTION", settings.name or ""),
            ("IngressRate", form.get("ingressRate", "1")),
            ("EgressRate", form.get("egressRate", "1")),
            ("priority", form.get("priority", "0")),
        ])  # fmt: skip
        _gs30x_check(content, ok_prefix=b"SUCCESS")
        time.sleep(_GS30X_PORT_SETTLE_SECONDS)

    def _set_switch_name(self, name: str) -> None:
        content = self._post("switch_name.cgi", [
            ("hash", self._vlan_form["hash"]),
            ("switch_name", name),
            ("SET_LANG", self._system_form["SET_LANG"]),
        ])  # fmt: skip
        _gs30x_check(content, ok_prefix=b"SWITCH NAME SUCCESS")


def _response(content: bytes) -> BaseResponse:
    response = BaseResponse()
    response.status_code = 200
    response.content = content
    return response


def _gs30x_check(content: bytes, ok_prefix: bytes) -> None:
    text = content.strip()
    if text.startswith(ok_prefix):
        return
    if text.startswith(b"CHECK HASH FAILED"):
        raise WriteError("switch rejected the session hash")
    raise WriteError(f"switch answered {text[:80]!r}")


def _gs30x_vlan_form(content: bytes) -> dict[str, str]:
    tree = html.fromstring(content)
    form = {
        "hash": _first(tree.xpath('//input[@id="hash"]/@value')),
        "vlanNum": _first(tree.xpath('//input[@id="vlanNum"]/@value')),
        "voVid": _first(tree.xpath('//input[@id="voVid"]/@value')) or "0",
        "mgmtVlan": _first(tree.xpath('//input[@name="mgmtVlan"]/@value')) or "1",
        "cos": (_first(tree.xpath('//*[@id="cos"]/text()')) or "6").strip(),
    }
    if not form["hash"] or not form["vlanNum"]:
        raise WriteError("VLAN page lacks hash or vlanNum")
    return form


def _gs30x_system_form(content: bytes) -> dict[str, str]:
    """Switch name and UI language from the dashboard; the name form posts both."""
    tree = html.fromstring(content)
    lang = _first(tree.xpath('//input[@name="SET_LANG"]/@value'))
    if not lang:
        raise WriteError("dashboard lacks the language field of the name form")
    return {
        "switch_name": _first(tree.xpath('//input[@id="switchName"]/@value')) or "",
        "SET_LANG": lang,
    }


def _gs30x_port_forms(content: bytes) -> dict[int, dict[str, str]]:
    tree = html.fromstring(content)
    forms = {}
    for block in tree.xpath('//li[contains(@class, "list_item")]'):
        values = {
            cls: _first(block.xpath(f'.//input[@class="{cls}"]/@value'))
            for cls in ("port", "portName", "Speed", "ingressRate", "egressRate", "flowCtr")
        }
        priority = block.xpath('.//input[contains(@class, "priority")]/@value')
        if priority:
            values["priority"] = priority[0]
        if values["port"] and values["port"].isdigit():
            forms[int(values["port"])] = {k: v for k, v in values.items() if v is not None}
    return forms


def _first(values: list[Any]) -> str | None:
    return str(values[0]) if values else None


# ---------------------------------------------------------------------------
# GS116Ev2 (JGS form protocol)
# ---------------------------------------------------------------------------

_JGS_SPEED_CODE = {
    Speed.DISABLED: "0",
    Speed.AUTO: "1",
    Speed.M10_HALF: "2",
    Speed.M10_FULL: "3",
    Speed.M100_HALF: "4",
    Speed.M100_FULL: "5",
}
_JGS_UNCHANGED = "255"
_VAR = r"var {name} = '([^']*)';"


class JgsWriter(ConfigWriter):
    """GS116Ev2 (and, unverified, other JGS-family models)."""

    caps = Capabilities(speeds=frozenset(_JGS_SPEED_CODE), max_vlans=100)

    _VLAN_CONF = "config/dot1based_advanced_vlan_conf.htm"
    _VLAN_MEMBERS = "config/dot1based_advanced_vlan_membership.htm"
    _PVID = "config/dot1based_advanced_pvid.htm"
    _STATUS = "config/status_status.htm"
    _SYSTEM = "config/status_switch_info.htm"

    def read_config(self) -> SwitchConfig:
        page = self._get(self._VLAN_MEMBERS).decode("utf-8", "replace")
        cfg = SwitchConfig()
        for entry in _jgs_var(page, "vlanMem").split(","):
            fields = entry.split("?")
            if not fields[0].isdigit():
                raise WriteError("VLAN membership page not understood (advanced 802.1Q enabled?)")
            cfg.vlans[int(fields[0])] = {
                port: Member(code)
                for port, code in enumerate(fields[1 : self.port_count + 1], start=1)
                if code in ("U", "T")
            }
        pvids = _jgs_var(page, "pvid").split("?")
        cfg.pvids = {p: int(v) for p, v in enumerate(pvids, start=1) if v.isdigit()}
        system = _jgs_system_info(self._get(self._SYSTEM).decode("utf-8", "replace"))
        cfg.switch_name = system.name or None
        status = self._get(self._STATUS).decode("utf-8", "replace")
        self._descriptions = {}
        for idx, fields in _jgs_port_entries(status).items():
            self._descriptions[idx] = fields[1]
            cfg.ports[idx] = PortSettings(speed=_jgs_speed(fields[3]), name=fields[1] or None)
        return cfg

    def _form_post(self, page: str, submit_id: str, fields: list[tuple[str, str]]) -> None:
        rand = _jgs_var(self._get(page).decode("utf-8", "replace"), "secureRand")
        content = self._post(
            page, [("submitId", submit_id), ("secureRand", rand), *fields, ("submitEnd", "")]
        )
        if pages.is_login_redirect(content):
            raise SessionDisplacedError("session lost while writing configuration")

    def _create_vlan(self, vid: int, members: dict[int, Member], before: SwitchConfig) -> None:
        # changeType must stay 0: changeType=1 switches the VLAN mode and
        # resets the whole VLAN configuration.
        self._form_post(
            self._VLAN_CONF,
            "vlanDot1VidCfg",
            [("changeType", "0"), ("addVid", str(vid)), ("confirmOK", "0")],
        )

    def _write_members(self, vid: int, members: dict[int, Member], before: SwitchConfig) -> None:
        member_mask = sum(1 << p for p in members)
        tag_mask = sum(1 << p for p, m in members.items() if m is Member.TAGGED)
        self._form_post(
            self._VLAN_MEMBERS,
            "vlanDot1TagCfg",
            [("vid", str(vid)), ("member", str(member_mask)), ("tag", str(tag_mask))],
        )

    def _delete_vlan(self, vid: int) -> None:
        self._form_post(
            self._VLAN_CONF, "vlanDot1VidCfg", [("changeType", "0"), ("delVid", str(vid))]
        )

    def _set_pvid(self, port: int, vid: int) -> None:
        self._form_post(self._PVID, "vlanDot1PvidCfg", [("port", str(port)), ("pvid", str(vid))])

    def _set_switch_name(self, name: str) -> None:
        # The GS116Ev2 has no separate name form: the name is posted together
        # with the management IP settings. Those are echoed exactly as the
        # switch reports them (what its own UI does for a name-only change)
        # and verified afterwards.
        page = self._get(self._SYSTEM).decode("utf-8", "replace")
        before = _jgs_system_info(page)
        if before.dhcp == "Enable":
            ip_fields = [("dhcp", "2")]
        else:
            ip_fields = [
                ("dhcp", "0"),
                ("ip", before.ip),
                ("subnet", before.netmask),
                ("gateway", before.gateway),
            ]
        content = self._post(self._SYSTEM, [
            ("submitId", "sysIp"),
            ("secureRand", _jgs_var(page, "secureRand")),
            ("swName", name),
            *ip_fields,
            ("submitEnd", ""),
        ])  # fmt: skip
        if pages.is_login_redirect(content):
            raise SessionDisplacedError("session lost while writing configuration")
        after = _jgs_system_info(self._get(self._SYSTEM).decode("utf-8", "replace"))
        if (after.dhcp, after.ip, after.netmask, after.gateway) != (
            before.dhcp, before.ip, before.netmask, before.gateway,
        ):  # fmt: skip
            _LOGGER.error(
                "host=%s management IP settings changed by the name update: %s -> %s",
                self._host, before, after,
            )  # fmt: skip
            raise WriteError("management IP settings changed by the name update")

    def _set_port(self, port: int, settings: PortSettings, before: SwitchConfig) -> None:
        description = (
            settings.name if settings.name is not None else self._descriptions.get(port, "")
        )
        self._form_post(
            self._STATUS,
            "sysStatus",
            [
                ("speed", _JGS_SPEED_CODE[settings.speed]),
                ("flow", _JGS_UNCHANGED),
                ("description", description),
                ("port", str(port)),
            ],
        )


@dataclass(frozen=True, slots=True)
class _JgsSystemInfo:
    """Fields of ``sysGeneInfor``: name and the management IP settings."""

    name: str
    dhcp: str  # "Enable" or "Disable"
    ip: str
    netmask: str
    gateway: str


def _jgs_system_info(page: str) -> _JgsSystemInfo:
    fields = _jgs_var(page, "sysGeneInfor").split("?")
    if len(fields) < 8 or fields[4] not in ("Enable", "Disable"):
        raise WriteError("switch info page not understood")
    name = fields[1].replace("@#q", "?")
    return _JgsSystemInfo(name, fields[4], fields[5], fields[6], fields[7])


def _jgs_var(page: str, name: str) -> str:
    match = re.search(_VAR.format(name=name), page)
    if not match:
        raise WriteError(f"page has no {name}")
    return match.group(1)


def _jgs_port_entries(page: str) -> dict[int, list[str]]:
    entries = {}
    for _idx, value in re.findall(r"portConfigEntry\[(\d+)\] = '([^']*)';", page):
        fields = value.split("?")
        if len(fields) >= 4 and fields[0].isdigit():
            entries[int(fields[0])] = fields
    return entries


def _jgs_speed(text: str) -> Speed:
    t = text.lower().replace(" ", "")
    if "disable" in t:
        return Speed.DISABLED
    if "auto" in t:
        return Speed.AUTO
    full = "full" in t
    if t.startswith("100"):
        return Speed.M100_FULL if full else Speed.M100_HALF
    if t.startswith("10"):
        return Speed.M10_FULL if full else Speed.M10_HALF
    return Speed.AUTO


WRITERS: dict[str, type[ConfigWriter]] = {"GS308Ev4": Gs30xWriter, "GS116Ev2": JgsWriter}


def advertised_speeds(caps: Capabilities) -> tuple[str, ...]:
    """Speed settings to offer in UniFi: what the switch accepts, plus 1000
    full, which the planner maps to auto (the ports are gigabit; leaving it
    out would make UniFi show them as 100 Mbit/s ports)."""
    speeds = (set(caps.speeds) | {Speed.M1000_FULL}) - {Speed.DISABLED}
    return tuple(s.value for s in Speed if s in speeds)
