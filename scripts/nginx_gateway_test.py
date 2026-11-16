"""Run the shipped nginx portal configuration against synthetic local backends.

Use an installed nginx, or Docker with the official nginx image. No production
configuration, account credential, SSH tunnel or published listener is touched.
"""

import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import unittest
from urllib.parse import parse_qs, urlsplit
import uuid


SOURCE = Path(__file__).resolve().parents[1] / "contrib/deck"
IDS = {"alpha": "a" * 32, "beta": "b" * 32, "gamma": "c" * 32}
TOKENS = {"alpha": "synthetic-central-bearer", "beta": "synthetic-beta-service", "gamma": "synthetic-gamma-service"}
COOKIE = "qswitch_session=synthetic-session"
ORIGIN = "https://gateway.example.test"


def unused_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class Backend:
    def __init__(self, name):
        self.name = name
        self.records = []
        self.lock = threading.Lock()
        backend = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                self.respond()

            def do_POST(self):
                self.respond()

            def reply(self, status, data, extra=None):
                raw = json.dumps(data).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                for key, value in (extra or {}).items():
                    self.send_header(key, value)
                self.end_headers()
                self.wfile.write(raw)

            def respond(self):
                size = int(self.headers.get("Content-Length", "0"))
                body = self.rfile.read(size) if size else b""
                parsed = urlsplit(self.path)
                query = parse_qs(parsed.query)
                record = {"method": self.command, "path": self.path, "headers": dict(self.headers), "body": body}
                with backend.lock:
                    backend.records.append(record)
                origin = self.headers.get("Origin")
                if origin and origin != ORIGIN:
                    return self.reply(403, {"error": "synthetic origin rejection"})
                if parsed.path == "/login":
                    if self.command == "POST":
                        return self.reply(303, {}, {"Set-Cookie": COOKIE + "; Path=/qswitch; HttpOnly", "Location": "/qswitch/"})
                    return self.reply(200, {"login": True})
                if parsed.path == "/logout":
                    return self.reply(303, {}, {"Set-Cookie": "qswitch_session=; Max-Age=0; Path=/qswitch", "Location": "/qswitch/login"})
                expected = query.get("expected_instance", [""])[0]
                incoming_id = self.headers.get("X-Qswitch-Instance", "")
                if expected and expected != IDS[name] or incoming_id and incoming_id != IDS[name]:
                    return self.reply(409, {"error": "instance mismatch; no operation was performed"})
                if parsed.path == "/api/session":
                    # Model qswitch's proxied request boundary. A missing
                    # forwarded host would wrongly make loopback auth succeed.
                    if self.headers.get("X-Forwarded-Host") != "gateway.example.test" or self.headers.get("X-Forwarded-Proto") != "https":
                        return self.reply(200, {"login_required": False})
                    authenticated = self.headers.get("Cookie") == COOKIE or self.headers.get("Authorization") == "Bearer " + TOKENS["alpha"]
                    if not authenticated and "text/html" in self.headers.get("Accept", ""):
                        return self.reply(303, {}, {"Location": "/qswitch/login"})
                    return self.reply(200 if authenticated else 401, {"login_required": True})
                if parsed.path == "/api/environments":
                    return self.reply(200, {"portal": self.headers.get("X-Qswitch-Portal")})
                if parsed.path != "/api/health" and self.headers.get("Authorization") != "Bearer " + TOKENS[name]:
                    return self.reply(401, {"error": "synthetic target authentication failed"})
                fault = query.get("fault", [""])[0]
                if fault == "disconnect":
                    self.connection.shutdown(socket.SHUT_RDWR)
                    self.connection.close()
                    return
                if fault == "auth":
                    return self.reply(401, {"error": "synthetic expired service token"}, {"X-Qswitch-Login-Required": "1"})
                if fault == "redirect":
                    return self.reply(302, {}, {"Location": "https://untrusted.example.test/", "Set-Cookie": "untrusted=1"})
                headers = {"Set-Cookie": "untrusted=1", "Location": "https://untrusted.example.test/", "X-Qswitch-Login-Required": "1"} if fault == "headers" else {}
                return self.reply(200, {"instance": {"id": IDS[name]}, "method": self.command, "path": self.path}, headers)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def requests(self, path=None):
        with self.lock:
            return [r for r in self.records if path is None or urlsplit(r["path"]).path == path]


class Gateway:
    nginx = shutil.which("nginx")
    image = os.environ.get("QSWITCH_NGINX_TEST_IMAGE", "nginx:1.28-alpine")

    @classmethod
    def prepare(cls):
        if cls.nginx:
            return
        if not shutil.which("docker"):
            raise RuntimeError("actual nginx is required: install nginx or provide Docker")
        subprocess.run(["docker", "version", "--format", "{{.Server.Version}}"], check=True, capture_output=True)
        result = subprocess.run(["docker", "image", "inspect", cls.image], capture_output=True)
        if result.returncode:
            subprocess.run(["docker", "pull", cls.image], check=True, capture_output=True)

    def __init__(self, backends, port_overrides=None, auth_port=None):
        self.temporary = tempfile.TemporaryDirectory(prefix="qswitch-nginx-test-")
        self.root = Path(self.temporary.name)
        self.name = "qswitch-nginx-test-" + uuid.uuid4().hex[:12]
        self.port = unused_port()
        self.process = None
        overrides = port_overrides or {}
        central = auth_port or backends["alpha"].port
        includes = self.root / "gateway"
        (includes / "secrets").mkdir(parents=True)
        def source(name):
            return (SOURCE / name).read_text().replace("/etc/nginx/qswitch-gateway", str(includes)).replace("http://127.0.0.1:7432", "http://127.0.0.1:" + str(central))
        for source_name, destination in (("nginx-qswitch-shell.conf", "shell.conf"), ("nginx-qswitch-gate.conf", "gate.conf"), ("nginx-qswitch-target.conf", "target.conf")):
            (includes / destination).write_text(source(source_name))
        instances = []
        for name, instance_id in IDS.items():
            rendered = source("nginx-qswitch-instance.conf.in").replace("@INSTANCE_NAME@", name).replace("@INSTANCE_ID@", instance_id).replace("@UPSTREAM_PORT@", str(overrides.get(name, backends[name].port)))
            instances.append(rendered)
            token_file = includes / "secrets" / (name + ".conf")
            token_file.write_text('proxy_set_header Authorization "Bearer ' + TOKENS[name] + '";\n')
            token_file.chmod(0o600)
        (includes / "instances.conf").write_text("\n".join(instances))
        self.conf = self.root / "nginx.conf"
        self.conf.write_text(f"pid {self.root}/nginx.pid;\nerror_log {self.root}/error.log info;\nevents {{}}\nhttp {{\naccess_log off;\nclient_body_temp_path {self.root}/body;\nproxy_temp_path {self.root}/proxy;\nfastcgi_temp_path {self.root}/fastcgi;\nuwsgi_temp_path {self.root}/uwsgi;\nscgi_temp_path {self.root}/scgi;\nserver {{\nlisten 127.0.0.1:{self.port};\nserver_name gateway.example.test;\n" + source("nginx-deck-qswitch.conf") + "\nlocation / { return 404; }\n}\n}\n")
        try:
            if self.nginx:
                command = [self.nginx, "-p", str(self.root), "-c", str(self.conf), "-g", "daemon off;"]
            else:
                command = ["docker", "run", "--rm", "--name", self.name, "--user", f"{os.getuid()}:{os.getgid()}", "--network", "host", "--mount", f"type=bind,src={self.root},dst={self.root}", "--entrypoint", "nginx", self.image, "-p", str(self.root), "-c", str(self.conf), "-g", "daemon off;"]
            self.process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            for _ in range(100):
                if self.process.poll() is not None:
                    _, error = self.process.communicate()
                    raise RuntimeError("nginx startup failed: " + error.decode())
                try:
                    self.request("/", authenticated=False)
                    break
                except (OSError, http.client.HTTPException):
                    time.sleep(0.05)
            else:
                raise RuntimeError("nginx test listener did not start")
        except BaseException:
            self.close()
            raise

    def request(self, path, method="GET", body=None, authenticated=True, headers=None):
        outgoing = {"Host": "gateway.example.test", "Origin": ORIGIN}
        if authenticated:
            outgoing["Cookie"] = COOKIE
        outgoing.update(headers or {})
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=8)
        try:
            connection.request(method, path, body=body, headers=outgoing)
            response = connection.getresponse()
            return response.status, {k.lower(): v for k, v in response.getheaders()}, response.read()
        finally:
            connection.close()

    def close(self):
        if self.process is not None:
            if not self.nginx:
                subprocess.run(["docker", "stop", "--time", "2", self.name], capture_output=True)
            elif self.process.poll() is None:
                self.process.terminate()
            try:
                self.process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.communicate()
        self.temporary.cleanup()


class GatewayIntegrationTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        Gateway.prepare()
        cls.backends = {name: Backend(name) for name in IDS}
        try:
            cls.gateway = Gateway(cls.backends)
        except BaseException:
            for backend in cls.backends.values():
                backend.close()
            raise

    @classmethod
    def tearDownClass(cls):
        cls.gateway.close()
        for backend in cls.backends.values():
            backend.close()

    def target(self, name, endpoint="overview"):
        return "/qswitch/api/instances/" + IDS[name] + "/" + endpoint

    def call(self, name, endpoint="overview", **kwargs):
        headers = {"X-Qswitch-Instance": IDS[name], **kwargs.pop("headers", {})}
        return self.gateway.request(self.target(name, endpoint), headers=headers, **kwargs)

    def test_cookie_login_and_logout(self):
        status, headers, _ = self.call("beta", authenticated=False)
        self.assertEqual(status, 401)
        self.assertEqual(headers.get("x-qswitch-login-required"), "1")
        status, headers, _ = self.gateway.request("/qswitch/login", method="POST", body="synthetic=1", authenticated=False)
        self.assertEqual(status, 303)
        self.assertIn("qswitch_session=", headers.get("set-cookie", ""))
        self.assertEqual(self.call("beta")[0], 200)
        status, headers, _ = self.gateway.request("/qswitch/logout", method="POST")
        self.assertEqual(status, 303)
        self.assertIn("Max-Age=0", headers.get("set-cookie", ""))
        self.assertEqual(self.call("beta", authenticated=False)[0], 401)

    def test_central_bearer_is_checked_then_replaced_per_target(self):
        status, _, _ = self.call("beta", authenticated=False, headers={"Authorization": "Bearer " + TOKENS["alpha"]})
        self.assertEqual(status, 200)
        received = self.backends["beta"].requests("/api/overview")[-1]["headers"]
        self.assertEqual(received.get("Authorization"), "Bearer " + TOKENS["beta"])
        self.assertNotIn("Cookie", received)
        status, headers, _ = self.call("beta", authenticated=False, headers={"Authorization": "Bearer " + TOKENS["beta"]})
        self.assertEqual(status, 401)
        self.assertEqual(headers.get("x-qswitch-login-required"), "1")

    def test_html_navigation_to_target_api_still_uses_json_auth_gate(self):
        status, headers, _ = self.call("beta", authenticated=False, headers={"Accept": "text/html,application/xhtml+xml"})
        self.assertEqual(status, 401)
        self.assertEqual(headers.get("x-qswitch-login-required"), "1")
        self.assertNotIn("location", headers)

    def test_authentication_precedes_route_and_header_errors(self):
        for path, extra in ((self.target("beta"), {}), (self.target("beta"), {"X-Qswitch-Instance": IDS["gamma"]}), ("/qswitch/api/instances/" + "f" * 32 + "/health", {}), ("/qswitch/api/instances", {}), ("/qswitch/api/instances/", {})):
            with self.subTest(path=path, extra=extra):
                status, headers, _ = self.gateway.request(path, authenticated=False, headers=extra)
                self.assertEqual(status, 401)
                self.assertEqual(headers.get("x-qswitch-login-required"), "1")

    def test_authenticated_missing_or_wrong_instance_cannot_write(self):
        count = len(self.backends["beta"].requests("/api/capture"))
        self.assertEqual(self.gateway.request(self.target("beta", "capture"), method="POST", body="{}")[0], 400)
        self.assertEqual(self.call("beta", "capture", method="POST", body="{}", headers={"X-Qswitch-Instance": IDS["gamma"]})[0], 409)
        self.assertEqual(len(self.backends["beta"].requests("/api/capture")), count)

    def test_csrf_is_rejected_by_central_gate(self):
        count = len(self.backends["beta"].requests("/api/capture"))
        status, headers, _ = self.call("beta", "capture", method="POST", body="{}", headers={"Origin": "https://other.example.test"})
        self.assertEqual(status, 403)
        self.assertNotIn("x-qswitch-login-required", headers)
        self.assertEqual(len(self.backends["beta"].requests("/api/capture")), count)

    def test_auth_subrequest_drops_target_data_but_target_preserves_request(self):
        body = b'{"tool":"synthetic","id":"fixture"}'
        endpoint = "probe?expected_instance=" + IDS["beta"] + "&id=synthetic%2Bvalue"
        status, _, _ = self.call("beta", endpoint, method="POST", body=body, headers={"Content-Type": "application/json", "X-Qswitch-Portal": "untrusted"})
        self.assertEqual(status, 200)
        auth = self.backends["alpha"].requests("/api/session")[-1]
        self.assertEqual(auth["path"], "/api/session")
        self.assertEqual(auth["method"], "GET")
        self.assertEqual(auth["body"], b"")
        self.assertEqual(auth["headers"].get("Accept"), "application/json")
        self.assertNotIn("X-Qswitch-Instance", auth["headers"])
        self.assertNotIn("X-Qswitch-Portal", auth["headers"])
        self.assertEqual(auth["headers"].get("Cookie"), COOKIE)
        self.assertEqual(auth["headers"].get("Origin"), ORIGIN)
        self.assertEqual(auth["headers"].get("X-Forwarded-Host"), "gateway.example.test")
        self.assertEqual(auth["headers"].get("X-Forwarded-Proto"), "https")
        self.assertEqual(auth["headers"].get("X-Forwarded-Prefix"), "/qswitch")
        target = self.backends["beta"].requests("/api/probe")[-1]
        self.assertEqual(target["method"], "POST")
        self.assertEqual(target["path"], "/api/" + endpoint)
        self.assertEqual(target["body"], body)
        self.assertEqual(target["headers"].get("X-Qswitch-Instance"), IDS["beta"])
        self.assertEqual(target["headers"].get("Origin"), ORIGIN)
        self.assertNotIn("Cookie", target["headers"])
        self.assertNotIn("X-Qswitch-Portal", target["headers"])

    def test_wrong_instance_at_configured_port_is_rejected(self):
        gateway = Gateway(self.backends, port_overrides={"gamma": self.backends["beta"].port})
        try:
            status, _, body = gateway.request(self.target("gamma", "capture"), method="POST", body="{}", headers={"X-Qswitch-Instance": IDS["gamma"]})
            self.assertEqual(status, 409)
            self.assertIn(b"instance mismatch", body)
        finally:
            gateway.close()

    def test_target_401_does_not_expire_central_login(self):
        status, headers, body = self.call("beta", "probe?fault=auth", method="POST", body="{}")
        self.assertEqual(status, 401)
        self.assertNotIn("x-qswitch-login-required", headers)
        self.assertIn("目标环境", json.loads(body)["error"])

    def test_target_cannot_set_cookies_redirect_or_set_login_marker(self):
        status, headers, _ = self.call("beta", "overview?fault=headers")
        self.assertEqual(status, 200)
        for key in ("set-cookie", "location", "x-qswitch-login-required"):
            self.assertNotIn(key, headers)
        status, headers, _ = self.call("beta", "overview?fault=redirect")
        self.assertEqual(status, 502)
        self.assertNotIn("location", headers)
        self.assertNotIn("set-cookie", headers)

    def test_unknown_environment_endpoint_and_retired_paths_are_not_compatible_routes(self):
        paths = ["/qswitch/api/instances", "/qswitch/api/instances/", "/qswitch/api/instances/" + "f" * 32 + "/health", self.target("beta", "session"), self.target("beta", "https://untrusted.example.test/"), "/qswitch/api/capture", "/qswitch-mini", "/qswitch-mini/", "/qswitch-book", "/qswitch-book/", "/qswitch/_session_gate"]
        for path in paths:
            with self.subTest(path=path):
                status, headers, _ = self.gateway.request(path, headers={"X-Qswitch-Instance": IDS["beta"]})
                self.assertEqual(status, 404)
                self.assertNotIn("location", headers)

    def test_dropped_post_response_is_not_replayed(self):
        count = len(self.backends["beta"].requests("/api/capture"))
        status, headers, body = self.call("beta", "capture?fault=disconnect", method="POST", body="{}")
        self.assertEqual(status, 503)
        self.assertNotIn("x-qswitch-login-required", headers)
        self.assertEqual(len(self.backends["beta"].requests("/api/capture")), count + 1)
        self.assertTrue(json.loads(body)["result_unknown"])
        self.assertIn("不要重复提交", json.loads(body)["error"])

    def test_offline_target_is_json_503(self):
        gateway = Gateway(self.backends, port_overrides={"gamma": unused_port()})
        try:
            status, headers, body = gateway.request(self.target("gamma", "health"), headers={"X-Qswitch-Instance": IDS["gamma"]})
            self.assertEqual(status, 503)
            self.assertEqual(headers.get("content-type"), "application/json")
            self.assertNotIn("x-qswitch-login-required", headers)
            self.assertIn("目标环境", json.loads(body)["error"])
            self.assertNotIn("result_unknown", json.loads(body))
        finally:
            gateway.close()

    def test_offline_auth_fails_closed_without_expiring_login(self):
        gateway = Gateway(self.backends, auth_port=unused_port())
        try:
            status, headers, body = gateway.request(self.target("beta"), headers={"X-Qswitch-Instance": IDS["beta"]})
            self.assertEqual(status, 503)
            self.assertNotIn("x-qswitch-login-required", headers)
            self.assertIn("统一登录", json.loads(body)["error"])
        finally:
            gateway.close()

    def test_portal_flag_is_only_set_on_canonical_shell_and_catalog(self):
        status, _, body = self.gateway.request("/qswitch/api/environments", headers={"X-Qswitch-Portal": "untrusted"})
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["portal"], "1")
        self.call("beta", headers={"X-Qswitch-Portal": "1"})
        self.assertNotIn("X-Qswitch-Portal", self.backends["beta"].requests("/api/overview")[-1]["headers"])


if __name__ == "__main__":
    unittest.main()
