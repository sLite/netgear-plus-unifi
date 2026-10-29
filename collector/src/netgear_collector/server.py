"""Internal JSON API consumed by the emulator (not exposed outside the compose network).

GET /healthz             liveness
GET /v1/switches         every switch's status and latest snapshot
POST /v1/switches/<id>/config  UniFi provisioning {"cfgversion", "system_cfg"} from the emulator
"""

from __future__ import annotations

import json
import logging
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

from .poller import StatusStore

_LOGGER = logging.getLogger(__name__)
MAX_BODY = 1 << 20


def make_server(host: str, port: int, store: StatusStore) -> ThreadingHTTPServer:
    class Handler(BaseHTTPRequestHandler):
        server_version = "netgear-collector"

        def do_GET(self) -> None:
            path = self.path.split("?", 1)[0].rstrip("/")
            if path == "/healthz":
                self._send(HTTPStatus.OK, {"status": "ok"})
            elif path == "/v1/switches":
                self._send(HTTPStatus.OK, {"switches": store.statuses()})
            else:
                self._send(HTTPStatus.NOT_FOUND, {"error": "not found"})

        def do_POST(self) -> None:
            path = self.path.split("?", 1)[0].rstrip("/")
            switch_id = path.removeprefix("/v1/switches/").removesuffix("/config")
            sync = store.syncs.get(switch_id)
            if not (path.startswith("/v1/switches/") and path.endswith("/config")) or not sync:
                self._send(HTTPStatus.NOT_FOUND, {"error": "unknown switch"})
                return
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                self._send(HTTPStatus.REQUEST_ENTITY_TOO_LARGE, {"error": "body too large"})
                return
            try:
                body = json.loads(self.rfile.read(length))
                cfgversion, system_cfg = str(body["cfgversion"]), str(body["system_cfg"])
            except (ValueError, KeyError, TypeError):
                self._send(HTTPStatus.BAD_REQUEST, {"error": "need cfgversion and system_cfg"})
                return
            sync.submit(cfgversion, system_cfg)
            self._send(HTTPStatus.ACCEPTED, sync.status())

        def _send(self, code: HTTPStatus, body: dict[str, Any]) -> None:
            raw = json.dumps(body).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, fmt: str, *args: Any) -> None:
            _LOGGER.debug("http %s - " + fmt, self.address_string(), *args)

    return ThreadingHTTPServer((host, port), Handler)
