"""Exercise the actual deploy.sh Python transaction without a host or credentials."""

import ast
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import URLError


TOOLS = ["codex", "grok", "cursor", "devin", "zcode", "kimi", "claude", "antigravity"]
OLD = "v0.0.1-fixture"
NEW = "v0.0.2-fixture"


def transaction_source():
    script = Path(__file__).with_name("deploy.sh").read_text()
    match = re.search(r'if ! on "python3 - \$release \$scope \$h1 \$h2" <<\'PY\'\n(.*?)\nPY', script, re.S)
    if match is None:
        raise AssertionError("deploy transaction heredoc not found")
    return compile(match.group(1), "deploy.sh:transaction", "exec")


class DeploymentTransactionTest(unittest.TestCase):
    def exercise(self, failure=None, mac_start=None):
        with tempfile.TemporaryDirectory(prefix="qswitch-deploy-test-") as temporary:
            home = Path(temporary)
            data, bindir = home / ".qswitch", home / ".local/bin"
            stage = data / ("deploy-" + NEW)
            bindir.mkdir(parents=True)
            stage.mkdir(parents=True)
            db = sqlite3.connect(data / "state.db")
            wal_database = failure == "readonly after stop"
            if wal_database:
                db.execute("pragma journal_mode=WAL")
            db.execute("create table pending_switch (tool text)")
            db.commit()
            if not wal_database:
                db.close()
            digests = []
            originals = {}
            for name in ("qswitch", "qswitchd"):
                originals[name] = (OLD + "\n" + name).encode()
                (bindir / name).write_bytes(originals[name])
                target = (NEW + "\n" + name).encode()
                (stage / name).write_bytes(target)
                digests.append(hashlib.sha256(target).hexdigest())
            if failure == "checksum":
                (stage / "qswitch").write_bytes(b"corrupt synthetic executable")

            service = {
                "version": OLD,
                "tearing_down": False,
                "teardown_polls": 999 if failure == "stuck bootout" else (2 if mac_start == "async bootout" else 0),
            }
            calls = []
            replaced = []
            real_replace = os.replace
            real_connect = sqlite3.connect
            stopped_database_checks = []

            def connect(database, *args, **kwargs):
                if wal_database and service["version"] is None:
                    self.assertFalse((data / "state.db-wal").exists())
                    self.assertFalse((data / "state.db-shm").exists())
                    stopped_database_checks.append(str(database))
                    # Reproduce Book's SQLite boundary without relying on the
                    # devbox SQLite build or filesystem behaving identically.
                    if "mode=ro" in str(database):
                        raise sqlite3.OperationalError("unable to open database file")
                return real_connect(database, *args, **kwargs)

            def replace(source, destination):
                source, destination = Path(source), Path(destination)
                if source.parent == stage and source.name in originals:
                    replaced.append(source.name)
                    if failure == "second replace" and source.name == "qswitchd":
                        raise OSError("synthetic second executable replacement failure")
                return real_replace(source, destination)

            def run(args, **kwargs):
                calls.append(tuple(args))
                def completed(code):
                    if code and kwargs.get("check"):
                        raise subprocess.CalledProcessError(code, args, output="", stderr="synthetic service startup failure")
                    return subprocess.CompletedProcess(args, code, stdout="", stderr="")

                if args[:2] == ["launchctl", "bootout"]:
                    self.assertEqual(args[2:], ["gui/501/com.qswitch"])
                    if service["teardown_polls"] > 0:
                        service["tearing_down"] = True
                    else:
                        service["version"] = None
                    return completed(0)
                if args[:2] == ["launchctl", "bootstrap"]:
                    self.assertEqual(args[2:], ["gui/501", str(home / "Library/LaunchAgents/com.qswitch.plist")])
                    if service["tearing_down"]:
                        return completed(5)
                    if mac_start == "async bootout":
                        service["version"] = (bindir / "qswitchd").read_text().splitlines()[0]
                        return completed(0)
                    if mac_start == "already loaded":
                        service["version"] = (bindir / "qswitchd").read_text().splitlines()[0]
                    return completed(5)
                if args[:2] == ["launchctl", "print"]:
                    self.assertEqual(args[2:], ["gui/501/com.qswitch"])
                    if service["tearing_down"]:
                        service["teardown_polls"] -= 1
                        if service["teardown_polls"] <= 0:
                            service["tearing_down"] = False
                            service["version"] = None
                            return completed(113)
                        return completed(0)
                    return completed(0 if service["version"] is not None else 113)
                if args[:1] == ["sudo"]:
                    self.assertEqual(args, ["sudo", "-n", "launchctl", "asuser", "501", "launchctl", "bootstrap", "gui/501", str(home / "Library/LaunchAgents/com.qswitch.plist")])
                    if mac_start == "both fail":
                        return completed(5)
                    service["version"] = (bindir / "qswitchd").read_text().splitlines()[0]
                    return completed(0)
                if args[:3] == ["systemctl", "--user", "stop"]:
                    service["version"] = None
                    if wal_database:
                        db.close()  # Last daemon connection removes WAL/SHM.
                    return subprocess.CompletedProcess(args, 0, stdout="", stderr="")
                if args[:3] == ["systemctl", "--user", "start"]:
                    service["version"] = (bindir / "qswitchd").read_text().splitlines()[0]
                    return subprocess.CompletedProcess(args, 0, stdout="", stderr="")
                executable = Path(args[0])
                if executable.parent not in (bindir, stage) or args[1:] not in (["version"], ["--version"]):
                    raise AssertionError("unexpected subprocess: " + repr(args))
                return subprocess.CompletedProcess(args, 0, stdout=executable.read_text().splitlines()[0] + "\n", stderr="")

            def urlopen(url, **kwargs):
                if service["tearing_down"] and failure != "stuck bootout":
                    service["tearing_down"] = False
                    service["version"] = None
                version = service["version"]
                if version is None or failure == "health" and version == NEW:
                    raise URLError("synthetic stopped or unhealthy service")
                tools = ["grok"] if failure == "scope" and version == NEW else TOOLS
                if url == "http://127.0.0.1:7432/api/health":
                    response = {"version": version, "instance": {"id": "fixture-instance", "managed_tools": tools}}
                elif url == "http://127.0.0.1:7432/api/overview":
                    response = {"tools": [{"tool": tool} for tool in tools]}
                    if failure == "derived":
                        response["tools"].append({"tool": "grokbot", "derived_from": "cursor", "accounts": [{"tool": "cursor", "derived": True}]})
                    if failure == "bad derived":
                        response["tools"].append({"tool": "grokbot", "derived_from": "grok"})
                    if failure == "unexpected tool":
                        response["tools"].append({"tool": "unmanaged-fixture"})
                else:
                    raise AssertionError("unexpected network request: " + url)
                return io.BytesIO(json.dumps(response).encode())

            output = io.StringIO()
            exited = None
            with patch("pathlib.Path.home", return_value=home), patch("sys.argv", ["-", NEW, "all", *digests]), patch("sys.platform", "darwin" if mac_start else "linux"), patch("os.getuid", return_value=501), patch("subprocess.run", side_effect=run), patch("urllib.request.urlopen", side_effect=urlopen), patch("time.sleep"), patch("os.replace", side_effect=replace), patch("sqlite3.connect", side_effect=connect), contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
                try:
                    exec(transaction_source(), {"__name__": "__main__"})
                except (SystemExit, AssertionError) as error:
                    exited = error
                finally:
                    if wal_database:
                        db.close()

            if wal_database:
                self.assertTrue(stopped_database_checks, "stopped database check was not exercised")
            if failure in (None, "derived", "readonly after stop") and mac_start != "both fail":
                self.assertIsNone(exited, output.getvalue())
                self.assertEqual(service["version"], NEW)
                receipt = json.loads((data / "deployment.json").read_text())
                self.assertEqual(receipt["release"], NEW)
            else:
                self.assertIsNotNone(exited, "fault was accepted as successful deployment")
                for name, previous in originals.items():
                    self.assertEqual((bindir / name).read_bytes(), previous, "partial upgrade was not rolled back")
                self.assertEqual(service["version"], None if mac_start == "both fail" else OLD, "unexpected recovery service state")
                self.assertFalse((data / "deployment.json").exists(), "failed deployment wrote a success receipt")
            if mac_start == "both fail":
                self.assertTrue(stage.exists(), "failed recovery removed its staging directory")
                self.assertIn("rollback needs attention", output.getvalue())
                self.assertIn("current and GUI sessions", str(exited))
                self.assertNotIn('"result": "deployed"', output.getvalue())
            else:
                self.assertFalse(stage.exists(), "completed rollback left temporary release files")
            if failure == "checksum":
                self.assertIn("checksum", str(exited))
                self.assertFalse(calls, "invalid staged bytes reached native execution or service control")
            elif mac_start:
                self.assertIn(("launchctl", "bootout", "gui/501/com.qswitch"), calls)
                gui_calls = [args for args in calls if args[0] == "sudo"]
                if mac_start in ("already loaded", "async bootout") or failure == "stuck bootout":
                    self.assertFalse(gui_calls, "an already-loaded, direct-bootstrap, or unstopped job used GUI fallback")
                else:
                    self.assertEqual(len(gui_calls), 2 if failure or mac_start == "both fail" else 1, "GUI fallback was not shared by apply and rollback")
            else:
                self.assertIn(("systemctl", "--user", "stop", "qswitchd.service"), calls)
            if failure == "stuck bootout":
                self.assertEqual(replaced, [], "binaries were replaced before launchd finished unloading the old job")
            if failure == "second replace":
                self.assertEqual(replaced, ["qswitch", "qswitchd"], "fault did not occur after the first executable changed")

    def test_success(self):
        self.exercise()

    def test_mac_bootstrap_uses_noninteractive_gui_session_fallback(self):
        self.exercise(mac_start="gui fallback")

    def test_mac_bootout_waits_for_async_launchd_teardown(self):
        self.exercise(mac_start="async bootout")

    def test_mac_stuck_bootout_aborts_before_replacing_binaries(self):
        self.exercise("stuck bootout", mac_start="async bootout")

    def test_mac_failed_bootstraps_do_not_report_success(self):
        self.exercise(mac_start="both fail")

    def test_mac_rollback_uses_gui_session_fallback(self):
        self.exercise("health", mac_start="gui fallback")

    def test_mac_already_loaded_job_is_not_bootstrapped_again(self):
        self.exercise(mac_start="already loaded")

    def test_stopped_database_without_wal_can_be_checked(self):
        self.exercise("readonly after stop")

    def test_second_replace_failure_restores_both_binaries(self):
        self.exercise("second replace")

    def test_all_scope_requires_every_tool(self):
        self.exercise("scope")

    def test_health_failure_restores_previous_service(self):
        self.exercise("health")

    def test_cursor_derived_bot_is_not_an_extra_worker(self):
        self.exercise("derived")

    def test_unknown_extra_tool_is_rejected(self):
        self.exercise("unexpected tool")

    def test_wrong_derived_parent_is_rejected(self):
        self.exercise("bad derived")

    def test_preflight_failure_cleans_stage_without_stopping_service(self):
        self.exercise("checksum")


class DeploymentDatabaseSafetyTest(unittest.TestCase):
    def safe(self, data):
        # Extract the actual transaction's function, without running service or
        # binary operations. This keeps the SQLite contract tied to production.
        script = Path(__file__).with_name("deploy.sh").read_text()
        body = re.search(r'if ! on "python3 - \$release \$scope \$h1 \$h2" <<\'PY\'\n(.*?)\nPY', script, re.S)
        self.assertIsNotNone(body)
        tree = ast.parse(body.group(1))
        function = next(node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name == "safe")
        namespace = {"sqlite3": sqlite3, "data": data}
        exec(compile(ast.Module(body=[function], type_ignores=[]), "deploy.sh:safe", "exec"), namespace)
        return namespace["safe"]

    def test_missing_database_is_not_created(self):
        with tempfile.TemporaryDirectory(prefix="qswitch-missing-db-test-") as temporary:
            data = Path(temporary)
            with self.assertRaises(sqlite3.OperationalError):
                self.safe(data)()
            self.assertFalse((data / "state.db").exists(), "safety inspection created a missing database")
            self.assertEqual(list(data.iterdir()), [], "missing database inspection left SQLite files")

    def test_query_only_blocks_writes_during_safety_inspection(self):
        with tempfile.TemporaryDirectory(prefix="qswitch-query-only-test-") as temporary:
            data = Path(temporary)
            with sqlite3.connect(data / "state.db") as database:
                database.execute("create table pending_switch (tool text)")
            database.close()
            checked = []
            test = self
            real_connect = sqlite3.connect

            class InspectConnection(sqlite3.Connection):
                def execute(self, sql, *args, **kwargs):
                    if sql.lstrip().lower().startswith("select"):
                        test.assertEqual(super().execute("pragma query_only").fetchone()[0], 1)
                        with test.assertRaises(sqlite3.OperationalError):
                            super().execute("insert into pending_switch values ('synthetic')")
                        checked.append(True)
                    return super().execute(sql, *args, **kwargs)

            def connect(database, *args, **kwargs):
                return real_connect(database, *args, factory=InspectConnection, **kwargs)

            with patch("sqlite3.connect", side_effect=connect):
                self.safe(data)()
            self.assertTrue(checked, "safe() did not query the actual database")
            with real_connect(data / "state.db") as database:
                self.assertEqual(database.execute("select count(*) from pending_switch").fetchone()[0], 0)
            database.close()


if __name__ == "__main__":
    unittest.main()
