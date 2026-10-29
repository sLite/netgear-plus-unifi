"""Parsers for NETGEAR page details py-netgear-plus does not expose.

The library's parsers remain the primary source; these functions only fill
gaps, keyed by parser family:

* GS108Ev4 family (GS108Ev4, GS308Ev4): the statistics page carries each
  counter as a (high, low) pair of 32-bit words. py-netgear-plus 0.7.x/main
  reads only the low word, so its byte counters wrap every 4 GiB.
  ``gs108ev4_counters`` combines both words. Evidence: the library's own
  recorded GS108Ev4 pages show the high word constant (e.g. 25) while the low
  word grows between samples.
* GS108Ev4 family dashboard: negotiated duplex ("1000M full"), port names and
  the hardware MAC.
* JGSxxx family (GS116Ev2, JGS516PE, JGS524Ev2): hardware MAC from
  ``sysGeneInfor``.
* Session loss: a displaced GS116Ev2 session receives HTTP 200 with a bare
  ``parent.location.href='/login.htm'`` script, which the library's
  authentication check does not recognise. ``is_login_redirect`` does.

All functions accept raw page bytes and return ``{}``/``None`` when the
expected markup is absent, so a firmware change degrades to "unknown" rather
than wrong data.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

from lxml import html

_WORD = 2**32
_MAC = re.compile(r"^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$")
_SPEEDS = {
    "10G": 10_000,
    "5G": 5_000,
    "2.5G": 2_500,
    "1G": 1_000,
    "1000M": 1_000,
    "100M": 100,
    "10M": 10,
}
_LOGIN_REDIRECT = re.compile(
    rb"location\.href\s*=\s*['\"]/?(?:login\.htm|login\.cgi|wmi/login)['\"]", re.IGNORECASE
)


@dataclass(frozen=True, slots=True)
class PortCounters:
    rx_bytes: int
    tx_bytes: int
    crc_errors: int


@dataclass(frozen=True, slots=True)
class PortExtras:
    full_duplex: bool | None
    name: str | None


def is_login_redirect(content: bytes) -> bool:
    """True when a page is only a script redirect to the login page."""
    return len(content) < 2048 and bool(_LOGIN_REDIRECT.search(content))


def parse_link_speed(text: str | None) -> int | None:
    """Map NETGEAR speed text ("1000M full", "100M", "No Speed") to Mbps.

    Returns 0 for "no link" texts and None for anything unrecognised.
    """
    if text is None:
        return None
    token = re.sub(r"(?i)\b(full|half)\b", "", text).strip().upper()
    if token in ("NO SPEED", "", "LINK DOWN", "DOWN"):
        return 0
    return _SPEEDS.get(token)


def parse_duplex(text: str | None) -> bool | None:
    """Return True/False for "... full"/"... half", None when not stated."""
    if not text:
        return None
    lowered = text.lower()
    if "full" in lowered:
        return True
    if "half" in lowered:
        return False
    return None


def gs108ev4_counters(content: bytes) -> dict[int, PortCounters]:
    """Parse 64-bit counters from the GS108Ev4-family portStatistics page.

    Each port row ``<li><span>N</span>...</li>`` is followed by six hidden
    inputs: rx_hi, rx_lo, tx_hi, tx_lo, crc_hi, crc_lo.
    """
    tree = html.fromstring(content)
    out: dict[int, PortCounters] = {}
    for li in tree.xpath("//li"):
        label = li.xpath("./span[1]/text()")
        if not label or not label[0].strip().isdigit():
            continue
        values = li.xpath("following-sibling::input[@type='hidden'][position() <= 6]/@value")
        if len(values) < 6:
            continue
        try:
            words = [int(v) for v in values[:6]]
        except ValueError:
            continue
        out[int(label[0].strip())] = PortCounters(
            rx_bytes=words[0] * _WORD + words[1],
            tx_bytes=words[2] * _WORD + words[3],
            crc_errors=words[4] * _WORD + words[5],
        )
    return out


def gs108ev4_port_extras(content: bytes) -> dict[int, PortExtras]:
    """Duplex and port names from the GS108Ev4-family dashboard."""
    tree = html.fromstring(content)
    out: dict[int, PortExtras] = {}
    for block in tree.xpath('//li[contains(@class, "list_item")]'):
        port = block.xpath('.//input[@class="port"]/@value')
        if not port or not port[0].isdigit():
            continue
        linked = block.xpath('.//input[@class="LinkedSpeed"]/@value')
        name = block.xpath('.//input[@class="portName"]/@value')
        out[int(port[0])] = PortExtras(
            full_duplex=parse_duplex(linked[0]) if linked else None,
            name=(name[0].strip() or None) if name else None,
        )
    return out


def gs108ev4_hardware_mac(content: bytes) -> str | None:
    """The switch MAC shown on the GS108Ev4-family dashboard (label ml678)."""
    tree = html.fromstring(content)
    titles = tree.xpath('//div[@class="hid_info_title"]/span/text()')
    values = tree.xpath('//div[@class="hid_info_title"]/following-sibling::div[1]/span/text()')
    for title, value in zip(titles, values, strict=False):
        if title.strip() == "ml678" and _MAC.match(value.strip()):
            return value.strip().lower()
    return None


def jgs_hardware_mac(content: bytes) -> str | None:
    """The switch MAC from the JGSxxx-family ``sysGeneInfor`` record."""
    match = re.search(r"sysGeneInfor = '([^']+)';", content.decode("utf-8", "replace"))
    if not match:
        return None
    fields = match.group(1).split("?")
    if len(fields) > 2 and _MAC.match(fields[2].strip()):
        return fields[2].strip().lower()
    return None
