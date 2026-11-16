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
    def exercise(self, failure=None):
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

            service = {"version": OLD}
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
            with patch("pathlib.Path.home", return_value=home), patch("sys.argv", ["-", NEW, "all", *digests]), patch("sys.platform", "linux"), patch("subprocess.run", side_effect=run), patch("urllib.request.urlopen", side_effect=urlopen), patch("time.sleep"), patch("os.replace", side_effect=replace), patch("sqlite3.connect", side_effect=connect), contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
                try:
                    exec(transaction_source(), {"__name__": "__main__"})
                except (SystemExit, AssertionError) as error:
                    exited = error
                finally:
                    if wal_database:
                        db.close()

            if wal_database:
                self.assertTrue(stopped_database_checks, "stopped database check was not exercised")
            if failure in (None, "derived", "readonly after stop"):
                self.assertIsNone(exited, output.getvalue())
                self.assertEqual(service["version"], NEW)
                receipt = json.loads((data / "deployment.json").read_text())
                self.assertEqual(receipt["release"], NEW)
            else:
                self.assertIsNotNone(exited, "fault was accepted as successful deployment")
                for name, previous in originals.items():
                    self.assertEqual((bindir / name).read_bytes(), previous, "partial upgrade was not rolled back")
                self.assertEqual(service["version"], OLD, "previous daemon was not restarted")
                self.assertFalse((data / "deployment.json").exists(), "failed deployment wrote a success receipt")
            self.assertFalse(stage.exists(), "completed rollback left temporary release files")
            if failure == "checksum":
                self.assertIn("checksum", str(exited))
                self.assertFalse(calls, "invalid staged bytes reached native execution or service control")
            else:
                self.assertIn(("systemctl", "--user", "stop", "qswitchd.service"), calls)
            if failure == "second replace":
                self.assertEqual(replaced, ["qswitch", "qswitchd"], "fault did not occur after the first executable changed")

    def test_success(self):
        self.exercise()

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
