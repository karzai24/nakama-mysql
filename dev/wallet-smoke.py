#!/usr/bin/env python3
"""Use the temporary local Lua fixture without printing credentials or tokens."""
import base64
import json
from pathlib import Path
import urllib.error
import urllib.request
import uuid

root = Path(__file__).resolve().parents[1]
env = dict(line.split("=", 1) for line in (root / ".env").read_text().splitlines() if line and not line.startswith("#"))
basic = "Basic " + base64.b64encode((env["NAKAMA_SERVER_KEY"] + ":").encode()).decode()

def request(method, path, data=None, auth=basic):
    req = urllib.request.Request("http://127.0.0.1:7350" + path, data=json.dumps(data).encode() if data is not None else None, method=method, headers={"Authorization":auth,"Content-Type":"application/json"})
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            content = response.read()
    except urllib.error.HTTPError as error:
        # Server response is useful for assertion diagnosis; credentials are never included.
        raise RuntimeError(f"{method} {path}: HTTP {error.code}: {error.read().decode()}") from None
    return json.loads(content) if content else {}

session = request("POST", "/v2/account/authenticate/device?create=true", {"id":"local-wallet-test-" + uuid.uuid4().hex})
auth = "Bearer " + session["token"]
try:
    result = request("POST", "/v2/rpc/local_wallet_contract", "", auth)
    assert json.loads(result["payload"])["ok"]
    account = request("GET", "/v2/account", auth=auth)
    assert json.loads(account["wallet"])["coins"] == 5
    assert account["user"]["display_name"] == "wallet committed"
    print("PASS real HTTP -> Lua wallet/batch/ledger/MultiUpdate, rollback and account persistence")
finally:
    request("DELETE", "/v2/account", auth=auth)
