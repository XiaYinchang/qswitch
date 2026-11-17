#!/usr/bin/env python3
"""Exercise control-plane reporting with two isolated, real Linux daemons.

Usage: python3 scripts/reporting_smoke_test.py --binary /path/to/qswitchd
No provider accounts are installed. All state and credentials are synthetic;
the only network traffic is between the two loopback listeners. Linux keeps
this process test away from macOS Keychain and Windows Credential Manager.
"""

import argparse
import contextlib
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


HUB_ID = "a" * 32
NODE_ID = "b" * 32
GATEWAY_HOST = "reporting.example.test"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def private_write(path, value):
    with open(path, "w", encoding="utf-8", opener=lambda p, f: os.open(p, f, 0o600)) as out:
        out.write(value)


def unused_ports():
    # Keep both reservations open until two distinct ports have been selected.
    with socket.socket() as first, socket.socket() as second:
        first.bind(("127.0.0.1", 0))
        second.bind(("127.0.0.1", 0))
        return first.getsockname()[1], second.getsockname()[1]


def request(url, *, headers=None, method="GET", timeout=2, expected=200):
    body = b"{}" if method == "POST" else None
    req = urllib.request.Request(url, data=body, headers=headers or {}, method=method)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    try:
        response = HTTP.open(req, timeout=timeout)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        require(response.status == expected,
                f"{method} {req.selector}: expected HTTP {expected}, got {response.status}")
        raw = response.read(2 * 1024 * 1024 + 1)
        require(len(raw) <= 2 * 1024 * 1024, "unexpectedly large response")
        # Management admission may return a plain-text 401 before its JSON handler.
        return json.loads(raw) if raw and expected == 200 else None


class Daemon:
    def __init__(self, binary, root, name, instance_id, port, hub_port):
        self.binary = binary
        self.root = root / name
        self.home = self.root / "home"
        self.data = self.root / "data"
        self.home.mkdir(parents=True, mode=0o700)
        self.data.mkdir(mode=0o700)
        self.name = name
        self.instance_id = instance_id
        self.url = f"http://127.0.0.1:{port}"
        self.admin_token = secrets.token_hex(32)
        self.report_token = secrets.token_hex(32)
        self.process = None
        private_write(self.data / "instance-id", instance_id + "\n")
        private_write(self.data / "config.toml", f"""[instance]
name = "{name}"
[general]
auto_switch = false
[notify]
enabled = false
[web]
enabled = true
addr = "127.0.0.1:{port}"
token = "{self.admin_token}"
[reporting]
url = "http://127.0.0.1:{hub_port}"
token = "{self.report_token}"
""")

    def headers(self):
        return {
            "Authorization": "Bearer " + self.admin_token,
            "X-Qswitch-Instance": self.instance_id,
            "X-Forwarded-Host": GATEWAY_HOST,
            "X-Forwarded-Proto": "https",
            "X-Forwarded-Prefix": "/qswitch",
            "Origin": "https://" + GATEWAY_HOST,
        }

    def start(self):
        require(self.process is None or self.process.poll() is not None,
                f"{self.name} is already running")
        # Do not inherit user-specific provider paths, proxies, credentials or
        # QSWITCH_IN_TEST: exercise the real file-backed daemon in this sandbox.
        env = {"PATH": os.defpath, "LANG": "C", "HOME": str(self.home),
               "USERPROFILE": str(self.home), "QSWITCH_DIR": str(self.data)}
        for key, suffix in (("XDG_CONFIG_HOME", ".config"), ("XDG_DATA_HOME", ".local/share"),
                            ("XDG_CACHE_HOME", ".cache"), ("XDG_STATE_HOME", ".local/state"),
                            ("XDG_RUNTIME_DIR", ".run"), ("TMPDIR", ".tmp")):
            directory = self.home / suffix
            directory.mkdir(parents=True, exist_ok=True, mode=0o700)
            env[key] = str(directory)
        log_path = self.root / "daemon.log"
        with open(log_path, "a", opener=lambda p, f: os.open(p, f, 0o600)) as log:
            self.process = subprocess.Popen(
                [str(self.binary), "--tool", "zcode"], cwd=self.root, env=env,
                stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            self.check_running()
            try:
                health = request(self.url + "/api/health")
                require(health.get("status") == "ok", f"{self.name} health is not ok")
                require(health.get("instance", {}).get("id") == self.instance_id,
                        f"{self.name} health has the wrong instance")
                return
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                time.sleep(0.1)
        raise RuntimeError(f"{self.name} did not become healthy within 10 seconds")

    def check_running(self):
        require(self.process is not None and self.process.poll() is None,
                f"{self.name} exited unexpectedly")

    def stop(self):
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        require(self.process is None or self.process.poll() is not None,
                f"{self.name} could not be stopped")


def catalog(hub):
    hub.check_running()
    headers = hub.headers()
    headers["X-Qswitch-Portal"] = "1"
    result = request(hub.url + "/api/environments", headers=headers)
    require(result.get("control_plane") is True, "catalog is not the control plane")
    require(result.get("instance", {}).get("id") == HUB_ID, "wrong control-plane instance")
    entries = result.get("environments", [])
    require(len(entries) == 2, "catalog does not contain both configured environments")
    by_id = {entry["id"]: entry for entry in entries}
    require(set(by_id) == {HUB_ID, NODE_ID}, "catalog has unexpected environment IDs")
    for instance_id, entry in by_id.items():
        require(entry.get("api_base") == f"/qswitch/api/instances/{instance_id}/",
                "catalog returned the wrong management route")
    return by_id


def wait_for_reports(hub, node, *, newer_than=None, timeout=25):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        node.check_running()
        entries = catalog(hub)
        snapshots = [entry.get("snapshot", {}) for entry in entries.values()]
        if all(snapshot.get("overview") for snapshot in snapshots):
            if newer_than is None or entries[NODE_ID]["snapshot"]["observed_at"] > newer_than:
                for instance_id, entry in entries.items():
                    snapshot = entry["snapshot"]
                    overview = snapshot["overview"]
                    require(snapshot.get("stale") is False, "new report is unexpectedly stale")
                    require(snapshot.get("observed_at", 0) > 0 and snapshot.get("received_at", 0) > 0,
                            "report timestamps are missing")
                    require(overview.get("instance", {}).get("id") == instance_id,
                            "report identity does not match its environment")
                    require(overview["instance"].get("managed_tools") == ["zcode"],
                            "isolated daemon has the wrong tool scope")
                    tools = overview.get("tools", [])
                    require(len(tools) == 1 and tools[0].get("tool") == "zcode",
                            "isolated report has unexpected tools")
                    require(not tools[0].get("accounts") and not tools[0].get("live_id")
                            and tools[0].get("auto") is False,
                            "isolated report is not empty with automatic switching disabled")
                return entries
        time.sleep(0.2)
    raise RuntimeError("background reports did not arrive before the deadline")


def exercise(binary):
    with tempfile.TemporaryDirectory(prefix="qswitch-reporting-smoke-") as temporary:
        root = Path(temporary)
        hub_port, node_port = unused_ports()
        hub = Daemon(binary, root, "Control", HUB_ID, hub_port, hub_port)
        node = Daemon(binary, root, "Worker", NODE_ID, node_port, hub_port)
        private_write(hub.data / "environments.json", json.dumps([
            {"id": HUB_ID, "name": "Control"}, {"id": NODE_ID, "name": "Worker"}]))
        private_write(hub.data / "reporting-tokens.json", json.dumps({
            HUB_ID: hub.report_token, NODE_ID: node.report_token}))
        with contextlib.ExitStack() as cleanup:
            cleanup.callback(hub.stop)
            cleanup.callback(node.stop)
            hub.start()
            node.start()
            entries = wait_for_reports(hub, node)
            first = entries[NODE_ID]["snapshot"]["observed_at"]
            # Wait for an actual scheduled update, without using a manual command.
            entries = wait_for_reports(hub, node, newer_than=first)
            before = entries[NODE_ID]["snapshot"]["observed_at"]
            print("PASS: both nodes report automatically; scheduled reporting advances timestamps", flush=True)

            no_admin = hub.headers()
            no_admin.pop("Authorization")
            no_admin["X-Qswitch-Portal"] = "1"
            request(hub.url + "/api/environments", headers=no_admin, expected=401)
            wrong_admin = node.headers()
            wrong_admin["Authorization"] = "Bearer " + node.report_token
            request(node.url + "/api/report", method="POST", headers=wrong_admin, expected=401)
            ack = request(node.url + "/api/report", method="POST", headers=node.headers(), timeout=15)
            require(ack.get("reported") is True and ack.get("instance", {}).get("id") == NODE_ID,
                    "manual refresh did not acknowledge the target node")
            require(ack.get("observed_at", 0) > before, "manual refresh did not collect a newer report")
            current = catalog(hub)[NODE_ID]["snapshot"]
            require(current["observed_at"] >= ack["observed_at"],
                    "manual refresh was acknowledged before central storage")
            print("PASS: management authentication and manual refresh acknowledgement", flush=True)

            node.stop()
            saved = catalog(hub)[NODE_ID]["snapshot"]
            for _ in range(3):
                started = time.monotonic()
                offline = catalog(hub)[NODE_ID]["snapshot"]
                require(time.monotonic() - started < 1,
                        "catalog blocked while the worker was offline")
                require(offline == saved, "offline node lost or changed its stored snapshot")
            print("PASS: offline node retains its snapshot; catalog responds within one second", flush=True)

            hub.stop()
            hub.start()
            restored = catalog(hub)[NODE_ID]["snapshot"]
            require(restored == saved, "control-plane restart lost the persisted worker snapshot")
            print("PASS: control-plane restart preserves the stored snapshot", flush=True)
    require(not root.exists(), "temporary daemon state was not removed")
    print("PASS: child processes stopped and temporary state removed", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True, help="path to a built Linux qswitchd")
    args = parser.parse_args()
    require(sys.platform.startswith("linux"), "run this isolated daemon smoke test on Linux")
    binary = args.binary.resolve(strict=True)
    require(binary.is_file() and os.access(binary, os.X_OK), "--binary must be an executable file")
    exercise(binary)


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, ValueError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        sys.exit(1)
