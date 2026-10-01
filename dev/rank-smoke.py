#!/usr/bin/env python3
"""Real clients + Lua runtime, with a container restart to reload persisted ranks."""
import base64
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

root = Path(__file__).resolve().parents[1]
env = dict(line.split("=", 1) for line in (root / ".env").read_text().splitlines() if line and not line.startswith("#"))
basic = "Basic " + base64.b64encode((env["NAKAMA_SERVER_KEY"] + ":").encode()).decode()

def request(method, path, data=None, auth=basic, expected=200):
    req = urllib.request.Request("http://127.0.0.1:7350" + path, data=json.dumps(data).encode() if data is not None else None, method=method, headers={"Authorization":auth,"Content-Type":"application/json"})
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            status, content = response.status, response.read()
    except urllib.error.HTTPError as error:
        status, content = error.code, error.read()
    assert status in (expected if isinstance(expected, tuple) else (expected,)), f"{method} {path.split('?')[0]}: expected {expected}, got {status}: {content.decode()}"
    return json.loads(content) if content else {}

def rpc(auth, payload):
    return json.loads(request("POST", "/v2/rpc/local_rank_contract", payload, auth)["payload"])

auths = []
try:
    for _ in range(3):
        session = request("POST", "/v2/account/authenticate/device?create=true", {"id":"local-rank-test-" + uuid.uuid4().hex})
        auths.append("Bearer " + session["token"])
    ids = rpc(auths[0], "setup")
    board, tour, authoritative = ids["leaderboard"], ids["tournament"], ids["authoritative"]
    owners = [request("GET", "/v2/account", auth=auth)["user"]["id"] for auth in auths]
    for i, auth in enumerate(auths):
        r = request("POST", "/v2/leaderboard/" + board, {"score":str((i+1)*10), "subscore":"1", "metadata":"{}"}, auth)
        assert int(r["score"]) == (i+1)*10
    first = request("GET", "/v2/leaderboard/" + board + "?limit=1", auth=auths[0])
    second = request("GET", "/v2/leaderboard/" + board + "?limit=1&cursor=" + urllib.parse.quote(first["next_cursor"]), auth=auths[0])
    assert first["records"][0]["owner_id"] == owners[2] and second["records"][0]["owner_id"] == owners[1]
    around = request("GET", "/v2/leaderboard/" + board + "/owner/" + owners[1] + "?limit=3", auth=auths[0])
    assert len(around["records"]) == 3
    request("POST", "/v2/leaderboard/" + authoritative, {"score":"99"}, auths[0], 403)
    request("DELETE", "/v2/leaderboard/" + authoritative, auth=auths[0], expected=403)
    request("POST", "/v2/tournament/" + tour, {"score":"10"}, auths[0], 400)
    for auth in auths[:2]:
        request("POST", "/v2/tournament/" + tour + "/join", {}, auth)
        request("POST", "/v2/tournament/" + tour, {"score":"10"}, auth)
    request("POST", "/v2/tournament/" + tour + "/join", {}, auths[2], 400)
    request("POST", "/v2/tournament/" + tour, {"score":"20"}, auths[0])
    # Upstream PostgreSQL join-required exhaustion returns the unchanged row;
    # native MySQL returns FailedPrecondition. Both must retain the stored score.
    exhausted = request("POST", "/v2/tournament/" + tour, {"score":"30"}, auths[0], 400)
    if "score" in exhausted:
        assert int(exhausted["score"]) == 20
    rpc(auths[0], "attempt")
    request("POST", "/v2/tournament/" + tour, {"score":"30"}, auths[0])
    print("PASS runtime creation, scoring, ranks/pagination/haystack, authority, joins, capacity and attempt limits", flush=True)
    subprocess.run(["docker", "compose", "-f", "compose.dev.yml", "-f", "dev/compose.rank-test.yml", "up", "-d", "--no-deps", "--force-recreate", "--wait", "--wait-timeout", "120", "nakama"], cwd=root, check=True)
    # Rank preload is asynchronous; health can become ready first.
    deadline = time.monotonic() + 10
    while True:
        page = request("GET", "/v2/leaderboard/" + board + "?limit=10", auth=auths[0])
        if int(page.get("rank_count", 0)) == 3:
            break
        assert time.monotonic() < deadline, "persisted rank cache did not reload"
        time.sleep(0.1)
    assert [int(r["score"]) for r in page["records"]] == [30,20,10]
    page = request("GET", "/v2/tournament/" + tour + "?limit=10", auth=auths[0])
    assert len(page["records"]) == 2 and int(page["records"][0]["score"]) == 30
    request("DELETE", "/v2/tournament/" + tour, auth=auths[1])
    request("POST", "/v2/tournament/" + tour + "/join", {}, auths[2])
    request("DELETE", "/v2/leaderboard/" + board, auth=auths[1])
    print("PASS persisted definitions/scores/ranks after restart, record deletion and freed tournament capacity")
finally:
    try:
        if auths:
            rpc(auths[0], "cleanup")
    finally:
        for auth in auths:
            request("DELETE", "/v2/account", auth=auth)
