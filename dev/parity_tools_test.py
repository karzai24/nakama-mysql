"""Failure-path checks for CI progress and package-completeness reporting."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

TOOLS = Path(__file__).resolve().parent


class ParityToolsTest(unittest.TestCase):
    def test_live_output_preserves_failure_and_full_private_log(self):
        with tempfile.TemporaryDirectory() as tmp:
            log = Path(tmp) / "runner.log"
            events = [
                {"Action": "start", "Package": "example/server"},
                {"Action": "output", "Package": "example/server", "Test": "TestExample",
                 "Output": "fixture-secret-should-only-be-in-artifact"},
                {"Action": "fail", "Package": "example/server", "Test": "TestExample"},
                {"Action": "fail", "Package": "example/server", "Elapsed": 0.5},
            ]
            code = "import json,sys; print('[stage] START package-suite'); "
            code += "[print(json.dumps(e)) for e in " + repr(events) + "]; sys.exit(7)"
            result = subprocess.run([sys.executable, str(TOOLS / "parity-live.py"), str(log),
                                     sys.executable, "-c", code], capture_output=True, text=True)
            self.assertEqual(result.returncode, 7)
            self.assertIn("FAIL example/server/TestExample", result.stdout)
            self.assertNotIn("fixture-secret", result.stdout)
            self.assertIn("fixture-secret", log.read_text())

    def test_filtered_output_still_reports_heartbeat_then_success(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = subprocess.run([sys.executable, str(TOOLS / "parity-live.py"),
                                     str(Path(tmp) / "runner.log"), sys.executable, "-c",
                                     "import json,time; "
                                     "[(print(json.dumps({'Action':'output','Output':'fixture-payload'}), flush=True), time.sleep(1)) for _ in range(31)]"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0)
            self.assertIn("Still running: runner", result.stdout)
            self.assertIn("exit=0", result.stdout)

    def report(self, folder, events, expected, backend="mysql"):
        (folder / "tests.jsonl").write_text("".join(json.dumps(e) + "\n" for e in events))
        (folder / "expected-packages.txt").write_text("\n".join(expected) + "\n")
        result = subprocess.run([sys.executable, str(TOOLS / "parity-report.py"), backend,
                                 str(folder), "0"], capture_output=True, text=True)
        return result, json.loads((folder / "summary.json").read_text())

    def test_unexpected_database_skip_fails_and_explains_why(self):
        package = "github.com/heroiclabs/nakama/v3/server"
        with tempfile.TemporaryDirectory() as tmp:
            result, report = self.report(Path(tmp), [
                {"Action": "pass", "Package": package, "Test": "TestMySQLReleaseConfiguration"},
                {"Action": "skip", "Package": package, "Test": "TestMySQLStorageWrite"},
                {"Action": "pass", "Package": package},
            ], [package])
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("UNEXPECTED SKIP server.TestMySQLStorageWrite", result.stdout)
            self.assertIn("No approved skip reason", result.stdout)
            self.assertEqual(len(report["unexpected_skips"]), 1)

    def test_native_mysql_tests_must_run(self):
        package = "github.com/heroiclabs/nakama/v3/server"
        tests = ["TestMySQLReleaseConfiguration", "TestMySQLStorageWrite",
                 "TestMySQLIdentity", "TestMySQLWallet",
                 "TestMySQLAccountDeletion"]
        events = [{"Action": "pass", "Package": package, "Test": "TestMySQLConfiguration"}]
        events += [{"Action": "skip", "Package": package, "Test": test} for test in tests]
        events += [{"Action": "pass", "Package": package}]
        for backend in ["mysql"]:
            with self.subTest(backend=backend), tempfile.TemporaryDirectory() as tmp:
                result, report = self.report(Path(tmp), events, [package], backend)
                self.assertEqual(result.returncode, 1)
                self.assertEqual(len(report["unexpected_skips"]), 5)
                self.assertEqual(len(report["skipped_tests"]), 5)
                self.assertEqual(report["passed_top_level_tests"], 1)
                self.assertFalse(report["full_backend_parity_signed_off"])

    def test_new_native_test_is_not_implicitly_allowed_to_skip(self):
        package = "github.com/heroiclabs/nakama/v3/server"
        with tempfile.TemporaryDirectory() as tmp:
            result, report = self.report(Path(tmp), [
                {"Action": "skip", "Package": package, "Test": "TestMySQLFuture"},
                {"Action": "pass", "Package": package},
            ], [package], "mysql")
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(len(report["unexpected_skips"]), 1)

    def test_missing_package_cannot_pass_with_successful_exit(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, report = self.report(Path(tmp), [
                {"Action": "pass", "Package": "example/server", "Test": "TestExample"},
                {"Action": "pass", "Package": "example/server"},
            ], ["example/server", "example/skiplist"])
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(report["missing_packages"], ["example/skiplist"])

    def test_timeout_is_explicit_and_incomplete_tests_fail(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, report = self.report(Path(tmp), [
                {"Action": "run", "Package": "example/skiplist", "Test": "TestChaos"},
                {"Action": "output", "Package": "example/skiplist",
                 "Output": "panic: test timed out after 15m0s\n"},
                {"Action": "fail", "Package": "example/skiplist"},
            ], ["example/skiplist"])
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("TIMEOUT example/skiplist", result.stdout)
            self.assertEqual(report["failed_or_incomplete_tests"][0]["test"], "TestChaos")

    def test_complete_disjoint_package_runs_pass_without_parity_claim(self):
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp)
            (folder / "stages.tsv").write_text("package-suite\t2\t0\nskiplist-stress\t3\t0\n")
            events = []
            for package in ["example/server", "example/skiplist"]:
                events += [{"Action": "pass", "Package": package, "Test": "TestExample"},
                           {"Action": "pass", "Package": package}]
            result, report = self.report(folder, events, ["example/server", "example/skiplist"])
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(report["passed_top_level_tests"], 2)
            self.assertEqual(len(report["stages"]), 2)
            self.assertFalse(report["full_backend_parity_signed_off"])


if __name__ == "__main__":
    unittest.main()
