#!/usr/bin/env python3
"""Drive a DISPOSABLE UniFi Network test controller from the command line.

    test_controller.py devices                 list devices and their state
    test_controller.py ports MAC               show the port table UniFi stores
    test_controller.py adopt MAC [MAC...]      adopt pending devices
    test_controller.py wait MAC [MAC...]       wait until devices are connected
    test_controller.py forget MAC [MAC...]     remove devices from the controller
    test_controller.py network NAME VLAN       create a VLAN-only network (idempotent)
    test_controller.py network-delete VLAN     delete the network with that VLAN id
    test_controller.py port MAC PORT [native=VID] [tagged=all|none|VID,VID] [state=disabled]
                            [speed=auto|10|100|1000] [duplex=full|half] [name=TEXT]
                                               set a port override, like the port editor in the UI
    test_controller.py port-reset MAC PORT     remove a port override (back to defaults)

Environment: UNIFI_URL (default https://127.0.0.1:18443), UNIFI_USER (admin),
UNIFI_PASSWORD (default: the seeded test image's password), UNIFI_SITE.

Safety: refuses any controller outside loopback/docker networks unless
ALLOW_NON_TEST_CONTROLLER=1 is set, so it cannot touch the production DR7 by
accident. TLS verification is off because test controllers use self-signed
certificates.
"""

from __future__ import annotations

import http.cookiejar
import ipaddress
import json
import os
import ssl
import sys
import time
import urllib.parse
import urllib.request
from typing import Any

URL = os.environ.get("UNIFI_URL", "https://127.0.0.1:18443").rstrip("/")
USER = os.environ.get("UNIFI_USER", "admin")
PASSWORD = os.environ.get("UNIFI_PASSWORD", "unifi-containers-seeded")
SITE = os.environ.get("UNIFI_SITE", "default")
STATES = {0: "disconnected", 1: "connected", 2: "pending", 4: "upgrading", 5: "provisioning",
          6: "heartbeat-missed", 7: "adopt-failed", 10: "adopting"}  # fmt: skip


def guard() -> None:
    host = urllib.parse.urlparse(URL).hostname or ""
    try:
        ip = ipaddress.ip_address(host)
        test_net = ip.is_loopback or ip in ipaddress.ip_network("172.16.0.0/12")
    except ValueError:
        test_net = host in ("localhost", "unifi-controller")
    if not test_net and os.environ.get("ALLOW_NON_TEST_CONTROLLER") != "1":
        sys.exit(f"refusing {URL}: not a local test controller (set ALLOW_NON_TEST_CONTROLLER=1)")


class Controller:
    def __init__(self) -> None:
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        self._opener = urllib.request.build_opener(
            urllib.request.HTTPSHandler(context=ctx),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
        )
        self.call("/api/login", {"username": USER, "password": PASSWORD})

    def call(self, path: str, body: dict[str, Any] | None = None) -> Any:
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(URL + path, data=data, headers={"Content-Type": "application/json"})
        with self._opener.open(req, timeout=20) as resp:
            return json.load(resp).get("data")

    def devices(self) -> list[dict[str, Any]]:
        return self.call(f"/api/s/{SITE}/stat/device")

    def device(self, mac: str) -> dict[str, Any] | None:
        return next((d for d in self.devices() if d["mac"] == mac.lower()), None)

    def devmgr(self, cmd: str, mac: str) -> None:
        self.call(f"/api/s/{SITE}/cmd/devmgr", {"cmd": cmd, "mac": mac.lower()})

    def forget(self, mac: str) -> None:
        self.call(f"/api/s/{SITE}/cmd/sitemgr", {"cmd": "delete-device", "mac": mac.lower()})

    def put(self, path: str, body: dict[str, Any]) -> Any:
        req = urllib.request.Request(
            URL + path, data=json.dumps(body).encode(),
            headers={"Content-Type": "application/json"}, method="PUT",
        )  # fmt: skip
        with self._opener.open(req, timeout=20) as resp:
            return json.load(resp).get("data")

    def networks(self) -> list[dict[str, Any]]:
        return self.call(f"/api/s/{SITE}/rest/networkconf")

    def network_id(self, vid: int) -> str:
        for n in self.networks():
            if (n.get("vlan") or 1) == vid and (vid != 1 or n.get("purpose") != "vlan-only"):
                return n["_id"]
        sys.exit(f"no network with VLAN {vid}; create it with: network NAME {vid}")

    def set_override(self, mac: str, port: int, override: dict[str, Any] | None) -> None:
        dev = self.device(mac)
        if dev is None:
            sys.exit(f"{mac} not found")
        overrides = [o for o in dev.get("port_overrides") or [] if o["port_idx"] != port]
        if override is not None:
            overrides.append({"port_idx": port, **override})
        self.put(f"/api/s/{SITE}/rest/device/{dev['_id']}", {"port_overrides": overrides})


def port_override(c: Controller, args: list[str]) -> dict[str, Any]:
    """Translate UniFi port-editor intents into a port_overrides entry."""
    opts = dict(a.split("=", 1) for a in args)
    o: dict[str, Any] = {"setting_preference": "manual"}
    if opts.get("state") == "disabled":
        o.update(forward="disabled", native_networkconf_id="", tagged_vlan_mgmt="block_all")
    else:
        o["native_networkconf_id"] = c.network_id(int(opts.get("native", "1")))
        tagged = opts.get("tagged", "all")
        if tagged == "all":
            o["tagged_vlan_mgmt"] = "auto"
        elif tagged == "none":
            o["tagged_vlan_mgmt"] = "block_all"
        else:
            keep = {c.network_id(int(v)) for v in tagged.split(",")}
            others = [n["_id"] for n in c.networks() if n["_id"] not in keep and n.get("vlan")]
            o.update(tagged_vlan_mgmt="custom", excluded_networkconf_ids=others)
    speed = opts.get("speed", "auto")
    if speed != "auto":
        o.update(autoneg=False, speed=int(speed), full_duplex=opts.get("duplex", "full") == "full")
    else:
        o["autoneg"] = True
    if "name" in opts:
        o["name"] = opts["name"]
    return o


def show_devices(c: Controller) -> None:
    for d in c.devices():
        up = sum(1 for p in d.get("port_table", []) if p.get("up"))
        print(f"{d['mac']}  {d.get('model', ''):8} {STATES.get(d.get('state'), d.get('state')):12} "
              f"{d.get('name') or d.get('hostname', ''):20} ip={d.get('ip')} ports_up={up}/"
              f"{len(d.get('port_table', []))} version={d.get('version')}")  # fmt: skip


def show_ports(c: Controller, mac: str) -> None:
    d = c.device(mac)
    if d is None:
        sys.exit(f"{mac} not found")
    for p in d.get("port_table", []):
        print(f"port {p['port_idx']:2} up={p.get('up')!s:5} speed={p.get('speed')!s:5} "
              f"fdx={p.get('full_duplex')!s:5} rx={p.get('rx_bytes')} tx={p.get('tx_bytes')} "
              f"rx/s={p.get('rx_bytes-r')} tx/s={p.get('tx_bytes-r')} poe_caps={p.get('poe_caps')} "
              f"name={p.get('name')!r}")  # fmt: skip


def wait_connected(c: Controller, macs: list[str], timeout: float = 300) -> None:
    deadline = time.monotonic() + timeout
    for mac in macs:
        while True:
            d = c.device(mac)
            state = d.get("state") if d else None
            if state == 1:
                print(f"{mac} connected")
                break
            if time.monotonic() > deadline:
                sys.exit(f"{mac} not connected (state {STATES.get(state, state)})")
            time.sleep(3)


def main() -> None:
    guard()
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    cmd, macs = sys.argv[1], sys.argv[2:]
    c = Controller()
    if cmd == "devices":
        show_devices(c)
    elif cmd == "ports" and len(macs) == 1:
        show_ports(c, macs[0])
    elif cmd == "adopt":
        for mac in macs:
            c.devmgr("adopt", mac)
            print(f"adopt requested for {mac}")
            time.sleep(2)  # controllers reject adoption bursts
    elif cmd == "wait":
        wait_connected(c, macs)
    elif cmd == "network" and len(macs) == 2:
        name, vid = macs[0], int(macs[1])
        if not any(n.get("vlan") == vid for n in c.networks()):
            body = {"name": name, "purpose": "vlan-only", "vlan_enabled": True, "vlan": vid}
            c.call(f"/api/s/{SITE}/rest/networkconf", body)
        print(f"network {name} VLAN {vid} present")
    elif cmd == "network-delete" and len(macs) == 1:
        net = c.network_id(int(macs[0]))
        req = urllib.request.Request(f"{URL}/api/s/{SITE}/rest/networkconf/{net}", method="DELETE")
        c._opener.open(req, timeout=20).close()
        print(f"network with VLAN {macs[0]} deleted")
    elif cmd == "port" and len(macs) >= 2:
        c.set_override(macs[0], int(macs[1]), port_override(c, macs[2:]))
        print(f"port {macs[1]} of {macs[0]} updated")
    elif cmd == "port-reset" and len(macs) == 2:
        c.set_override(macs[0], int(macs[1]), None)
        print(f"port {macs[1]} of {macs[0]} reset")
    elif cmd == "forget":
        for mac in macs:
            c.forget(mac)
            print(f"forgot {mac}")
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
