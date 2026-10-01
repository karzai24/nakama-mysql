"""Preserve command output, stream compact test progress, and retain failure status."""
import datetime
import json
from pathlib import Path
import queue
import subprocess
import sys
import threading
import time


def main():
    logfile, *command = sys.argv[1:]
    started = time.monotonic()
    phase = Path(logfile).stem
    last_visible = started

    def emit(message):
        nonlocal last_visible
        last_visible = time.monotonic()
        now = datetime.datetime.now(datetime.timezone.utc).strftime("%H:%M:%SZ")
        print(f"[{now}] {message.rstrip()}", flush=True)

    emit(f"START {phase}")
    proc = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            text=True, errors="replace", bufsize=1)
    lines = queue.Queue()

    def read_output():
        try:
            for line in proc.stdout:
                lines.put(line)
        finally:
            lines.put(None)

    threading.Thread(target=read_output, daemon=True).start()
    try:
        with Path(logfile).open("w") as log:
            while True:
                try:
                    line = lines.get(timeout=30)
                except queue.Empty:
                    emit(f"Still running: {phase}; command elapsed={int(time.monotonic()-started)}s")
                    continue
                if line is None:
                    break
                if time.monotonic() - last_visible >= 30:
                    emit(f"Still running: {phase}; command elapsed={int(time.monotonic()-started)}s")
                log.write(line)
                log.flush()
                try:
                    event = json.loads(line)
                except json.JSONDecodeError:
                    if line.startswith("[stage] START "):
                        phase = line.strip().removeprefix("[stage] START ")
                    emit(line)
                    continue
                if not isinstance(event, dict) or "Action" not in event:
                    # Non-test JSON remains in the artifact, not in public progress.
                    continue
                action = event["Action"]
                package = event.get("Package", event.get("ImportPath", ""))
                test = event.get("Test")
                output = event.get("Output", "")
                if not test and action in ("start", "pass", "fail", "skip"):
                    elapsed = event.get("Elapsed")
                    suffix = f" ({elapsed}s)" if elapsed is not None else ""
                    emit(f"{action.upper()} {package}{suffix}")
                elif test and (action in ("fail", "skip") or
                               (action == "pass" and package.endswith("/internal/skiplist"))):
                    emit(f"{action.upper()} {package}/{test}")
                elif "skiplist chaos seed=" in output or "panic: test timed out" in output or "WARNING: DATA RACE" in output:
                    emit(output)
                elif action in ("build-output", "build-fail"):
                    emit(output or f"{action.upper()} {package}")
        status = proc.wait()
    finally:
        if proc.poll() is None:
            proc.terminate()
            proc.wait()
        proc.stdout.close()
    emit(f"END {Path(logfile).stem} elapsed={int(time.monotonic()-started)}s exit={status}")
    return status if status >= 0 else 128-status


if __name__ == "__main__":
    sys.exit(main())
