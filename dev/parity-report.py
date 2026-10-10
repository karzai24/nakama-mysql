"""Summarize go test JSON without hiding failures, unparsed output or skipped tests."""
import csv
import json
import re
from pathlib import Path
import sys

backend, folder, command_status = sys.argv[1], Path(sys.argv[2]), int(sys.argv[3])
path = folder / "tests.jsonl"
if not path.exists():
    raise SystemExit(f"{backend}: test output missing; inspect migration.log, vet.log and runner.log")
outcomes = {}
packages = {}
unparsed = []
for line in path.read_text(errors="replace").splitlines():
    try:
        event = json.loads(line)
    except json.JSONDecodeError:
        unparsed.append(line)
        continue
    action, package, test = event.get("Action"), event.get("Package", ""), event.get("Test")
    if test and action in ("run", "pass", "skip", "fail"):
        outcomes[(package, test)] = action
    elif not test and action in ("start", "pass", "fail", "skip"):
        packages[package] = action
skips = [{"package": p, "test": t} for (p, t), state in outcomes.items() if state == "skip"]
# Known skips remain visible coverage gaps; they never become parity passes.
manifest = json.loads(Path(__file__).with_name("parity-skips.json").read_text())
def skip_reason(item):
    key = item["package"].removeprefix("github.com/heroiclabs/nakama/v3/") + "/" + item["test"]
    if key in manifest["all"]:
        return manifest["all"][key]
    return None
for item in skips:
    item["reason"] = skip_reason(item)
unexpected = [item for item in skips if item["reason"] is None]
failures = [{"package": p, "test": t, "state": state} for (p, t), state in outcomes.items()
            if state not in ("pass", "skip")]
package_failures = [p for p, state in packages.items() if state not in ("pass", "skip")]
packages_with_tests = {p for p, _ in outcomes}
expected_path = folder / "expected-packages.txt"
expected_packages = set(expected_path.read_text().splitlines()) if expected_path.exists() else set()
missing_packages = sorted(expected_packages - packages.keys())
# Older retained reports predate this inventory; new runner always supplies it.
timeouts = []
for line in path.read_text(errors="replace").splitlines():
    try:
        event = json.loads(line)
    except json.JSONDecodeError:
        continue
    if "panic: test timed out" in event.get("Output", ""):
        timeouts.append({"package": event.get("Package"), "message": event["Output"].strip()})
stages_path = folder / "stages.tsv"
stages = []
if stages_path.exists():
    for label, elapsed, status in csv.reader(stages_path.open(), delimiter="\t"):
        stages.append({"stage": label, "elapsed_seconds": int(elapsed), "exit": int(status)})
# The same backend may be checked against multiple pinned releases. Preserve the
# actual server version reported during migration, not just the requested image tag.
engine_versions = []
migration_log = folder / "migration.log"
if migration_log.exists():
    for line in migration_log.read_text(errors="replace").splitlines():
        try:
            record = json.loads(line)
        except json.JSONDecodeError:
            continue
        if record.get("msg") == "Database information" and record.get("version"):
            engine_versions.append(record["version"])
summary = {
    "backend": backend, "engine_versions": engine_versions, "command_exit": command_status,
    "passed_top_level_tests": sum(state == "pass" and "/" not in t for (_, t), state in outcomes.items()),
    "passed_subtests": sum(state == "pass" and "/" in t for (_, t), state in outcomes.items()),
    "skipped_tests": skips, "unexpected_skips": unexpected,
    "failed_or_incomplete_tests": failures, "failed_packages": package_failures,
    "missing_packages": missing_packages, "timeouts": timeouts, "stages": stages,
    "unparsed_output_lines": len(unparsed),
    "packages_without_tests": [p for p, state in packages.items()
                               if p not in packages_with_tests and state in ("pass", "skip")],
    "coverage_excluded_source_locations": ((folder / "coverage-excluded.txt").read_text().splitlines()
                                            if (folder / "coverage-excluded.txt").exists() else []),
    "report_recovery": (json.loads((folder / "report-recovery.json").read_text())
                        if (folder / "report-recovery.json").exists() else None),
    "full_backend_parity_signed_off": False,
}
(folder / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
(folder / "test-inventory.json").write_text(json.dumps([
    {"package": p, "test": t, "outcome": state} for (p, t), state in sorted(outcomes.items())
], indent=2) + "\n")
# Source execution is an audit aid, not proof of assertions or behavioral parity.
# Keep every function, including helpers, so uncovered adapters remain inspectable.
functions = []
coverage_path = folder / "functions.txt"
if coverage_path.exists():
    for line in coverage_path.read_text().splitlines():
        match = re.match(r"(.+\.go):(\d+):\s+(\S+)\s+([\d.]+)%$", line)
        if not match:
            continue
        source, line_number, name, coverage = match.groups()
        source = source.removeprefix("github.com/heroiclabs/nakama/v3/")
        filename = Path(source).name
        surface = "core_or_support"
        for prefix, label in (("runtime_go_nakama", "go_runtime"), ("runtime_lua_nakama", "lua_runtime"),
                              ("runtime_javascript_nakama", "javascript_runtime"), ("api_", "client_api"),
                              ("console_", "console"), ("pipeline_", "realtime"),
                              ("leaderboard_scheduler", "scheduler"), ("leaderboard_rank_cache", "rank_cache"),
                              ("storage_index", "storage_index")):
            if source.startswith("server/") and filename.startswith(prefix):
                surface = label
                break
        functions.append({"source": source, "line": int(line_number), "function": name,
                          "surface": surface, "statement_coverage_percent": float(coverage),
                          "contract_signoff": "pending"})
    with (folder / "function-inventory.csv").open("w", newline="") as output:
        writer = csv.DictWriter(output, fieldnames=["source", "line", "function", "surface",
                                                   "statement_coverage_percent", "contract_signoff"])
        writer.writeheader()
        writer.writerows(functions)
print(f"{backend}: {summary['passed_top_level_tests']} top-level tests and {summary['passed_subtests']} subtests passed; {len(skips)} skipped")
for item in skips:
    label = "SKIP" if item["reason"] else "UNEXPECTED SKIP"
    reason = item["reason"] or "No approved skip reason; the test must run or its backend restriction must be documented."
    print(f"  {label} {item['package'].rsplit('/', 1)[-1]}.{item['test']}: {reason}")
for item in failures:
    print(f"  {item['state'].upper()} {item['package'].rsplit('/', 1)[-1]}.{item['test']}")
for timeout in timeouts:
    print(f"  TIMEOUT {timeout['package']}: {timeout['message']}")
for package in missing_packages:
    print(f"  MISSING PACKAGE {package}")
for package in package_failures:
    print(f"  PACKAGE FAILURE {package}")
if unparsed:
    print(f"  {len(unparsed)} non-JSON diagnostics; inspect tests.jsonl")
if not outcomes:
    print("  No test outcomes were recorded")
raise SystemExit(bool(command_status or failures or package_failures or missing_packages or timeouts or unexpected or unparsed or not outcomes))
