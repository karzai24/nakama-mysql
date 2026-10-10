#!/usr/bin/env python3
"""Exercise the running local API without printing credentials or session tokens."""
import base64
import hashlib
import json
from pathlib import Path
import urllib.error
import urllib.parse
import urllib.request
import uuid

root = Path(__file__).resolve().parents[1]
env = dict(line.split("=", 1) for line in (root / ".env").read_text().splitlines() if line and not line.startswith("#"))
base = "http://127.0.0.1:7350"
basic = "Basic " + base64.b64encode((env["NAKAMA_SERVER_KEY"] + ":").encode()).decode()

def request(method, path, data=None, auth=basic, expected=200):
    body = json.dumps(data).encode() if data is not None else None
    req = urllib.request.Request(base + path, data=body, method=method, headers={"Authorization": auth, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            status, content = response.status, response.read()
    except urllib.error.HTTPError as error:
        status, content = error.code, error.read()
    assert status in (expected if isinstance(expected, tuple) else (expected,)), f"{method} {path.split('?')[0]}: expected {expected}, got {status}"
    return json.loads(content) if content else {}

device = "mysql-port-smoke-" + uuid.uuid4().hex
session = request("POST", "/v2/account/authenticate/device?create=true", {"id": device})
auth = "Bearer " + session["token"]
account = request("GET", "/v2/account", auth=auth)
owner = account["user"]["id"]
again = request("POST", "/v2/account/authenticate/device?create=false", {"id": device})
assert request("GET", "/v2/account", auth="Bearer " + again["token"])["user"]["id"] == owner
request("PUT", "/v2/account", {"display_name": "Local فارسی 🂡"}, auth)
assert request("GET", "/v2/account", auth=auth)["user"]["display_name"] == "Local فارسی 🂡"
refreshed = request("POST", "/v2/account/session/refresh", {"token": session["refresh_token"]})
assert request("GET", "/v2/account", auth="Bearer " + refreshed["token"])["user"]["id"] == owner
print("PASS device authentication, account read/update, session refresh")

# Exercise the public identity routes, including ownership conflicts and the
# inability to remove the last remaining login from an account.
def session_owner(session):
    return request("GET", "/v2/account", auth="Bearer " + session["token"])["user"]["id"]

custom_id = "custom-smoke-" + uuid.uuid4().hex
custom_session = request("POST", "/v2/account/authenticate/custom?create=true", {"id": custom_id})
assert custom_session["created"]
custom_owner = session_owner(custom_session)
assert session_owner(request("POST", "/v2/account/authenticate/custom?create=false", {"id": custom_id})) == custom_owner
request("POST", "/v2/account/unlink/custom", {"id": custom_id}, "Bearer " + custom_session["token"], expected=403)
request("POST", "/v2/account/link/custom", {"id": custom_id}, auth, expected=409)
linked_custom = "linked-custom-" + uuid.uuid4().hex
request("POST", "/v2/account/link/custom", {"id": linked_custom}, auth)
request("POST", "/v2/account/link/custom", {"id": linked_custom}, auth)
assert session_owner(request("POST", "/v2/account/authenticate/custom?create=false", {"id": linked_custom})) == owner
request("POST", "/v2/account/unlink/device", {"id": device}, auth)
request("POST", "/v2/account/unlink/custom", {"id": linked_custom}, auth, expected=403)
request("POST", "/v2/account/link/device", {"id": device}, auth)
second_device = "linked-device-" + uuid.uuid4().hex
request("POST", "/v2/account/link/device", {"id": second_device}, auth)
assert session_owner(request("POST", "/v2/account/authenticate/device?create=false", {"id": second_device})) == owner
request("POST", "/v2/account/unlink/device", {"id": second_device}, auth)
request("POST", "/v2/account/unlink/custom", {"id": linked_custom}, auth)
request("POST", "/v2/account/authenticate/custom?create=false", {"id": linked_custom}, expected=404)
print("PASS custom login and custom/device linking, ownership conflicts, last-identifier protection")

email = uuid.uuid4().hex + "@example.invalid"
password = uuid.uuid4().hex
email_username = "email_smoke_" + uuid.uuid4().hex
email_session = request("POST", "/v2/account/authenticate/email?create=true&username=" + email_username, {"email": email.upper(), "password": password})
assert email_session["created"]
email_owner = session_owner(email_session)
assert session_owner(request("POST", "/v2/account/authenticate/email?create=false", {"email": email, "password": password})) == email_owner
assert session_owner(request("POST", "/v2/account/authenticate/email?username=" + email_username, {"password": password})) == email_owner
request("POST", "/v2/account/authenticate/email?create=false", {"email": email, "password": "wrong-password"}, expected=401)
request("POST", "/v2/account/unlink/email", {"email": email}, "Bearer " + email_session["token"], expected=403)
request("POST", "/v2/account/link/email", {"email": email, "password": password}, auth, expected=409)
linked_email = uuid.uuid4().hex + "@example.invalid"
request("POST", "/v2/account/link/email", {"email": linked_email.upper(), "password": password}, auth)
assert session_owner(request("POST", "/v2/account/authenticate/email?create=false", {"email": linked_email, "password": password})) == owner
account = request("GET", "/v2/account", auth=auth)
assert account["email"] == linked_email
new_password = uuid.uuid4().hex
request("POST", "/v2/account/link/email", {"email": linked_email, "password": new_password}, auth)
request("POST", "/v2/account/authenticate/email?create=false", {"email": linked_email, "password": password}, expected=401)
assert session_owner(request("POST", "/v2/account/authenticate/email?username=" + account["user"]["username"], {"password": new_password})) == owner
request("POST", "/v2/account/unlink/email", {"email": linked_email.upper()}, auth)
request("POST", "/v2/account/authenticate/email?username=" + account["user"]["username"], {"password": new_password}, expected=401)
request("POST", "/v2/account/authenticate/email?create=false", {"email": linked_email, "password": new_password}, expected=404)
print("PASS email/username password login, normalized email linking, password rotation and unlink")

# Use the public API for the complete friendship path. The three accounts are
# unique to this run and are removed by the account-deletion checks below.
custom_auth = "Bearer " + custom_session["token"]
email_auth = "Bearer " + email_session["token"]
def friendship_states(bearer):
    return {f["user"]["id"]: f.get("state", 0) for f in request("GET", "/v2/friend", auth=bearer).get("friends", [])}

def notification_count(bearer, code):
    return sum(n["code"] == code for n in request("GET", "/v2/notification?limit=100", auth=bearer).get("notifications", []))

metadata = urllib.parse.quote(json.dumps({"source": "local-smoke", "nested": {"label": "فارسی"}}))
request("POST", "/v2/friend?ids=" + custom_owner.upper() + "&metadata=" + metadata, auth=auth)
assert friendship_states(auth) == {custom_owner: 1}
assert friendship_states(custom_auth) == {owner: 2}
assert notification_count(custom_auth, -2) == 1
request("POST", "/v2/friend?ids=" + custom_owner, auth=auth)
assert notification_count(custom_auth, -2) == 1
request("POST", "/v2/friend?ids=" + owner, auth=custom_auth)
assert friendship_states(auth) == {custom_owner: 0}
assert friendship_states(custom_auth) == {owner: 0}
assert notification_count(auth, -3) == 1
request("POST", "/v2/friend?usernames=" + email_username, auth=custom_auth)
request("POST", "/v2/friend?ids=" + custom_owner, auth=email_auth)
fof = request("GET", "/v2/friend/friends?limit=10", auth=auth)["friends_of_friends"]
assert [(f["referrer"], f["user"]["id"]) for f in fof] == [(custom_owner, email_owner)]
first_page = request("GET", "/v2/friend?limit=1&state=0", auth=custom_auth)
second_page = request("GET", "/v2/friend?limit=1&state=0&cursor=" + urllib.parse.quote(first_page["cursor"]), auth=custom_auth)
assert {first_page["friends"][0]["user"]["id"], second_page["friends"][0]["user"]["id"]} == {owner, email_owner}
request("POST", "/v2/friend/block?ids=" + custom_owner, auth=auth)
assert friendship_states(auth) == {custom_owner: 3}
assert owner not in friendship_states(custom_auth)
request("POST", "/v2/friend?ids=" + owner, auth=custom_auth)
assert owner not in friendship_states(custom_auth)
request("POST", "/v2/friend/block?ids=" + owner, auth=custom_auth)
request("DELETE", "/v2/friend?ids=" + custom_owner, auth=auth)
assert friendship_states(custom_auth)[owner] == 3
request("DELETE", "/v2/friend?ids=" + owner, auth=custom_auth)
request("POST", "/v2/friend?ids=" + custom_owner, auth=auth)
request("POST", "/v2/friend?ids=" + owner, auth=custom_auth)
request("DELETE", "/v2/friend?ids=" + custom_owner, auth=auth)
assert owner not in friendship_states(custom_auth)
assert notification_count(custom_auth, -9) == 1
print("PASS friendship request/accept, metadata, notifications, pagination, friends-of-friends, blocks and removal")

# Group lifecycle through public routes, including closed requests and permissions.
group_name = "group-smoke-" + uuid.uuid4().hex
group = request("POST", "/v2/group", {"name": group_name, "open": False, "max_count": 3, "lang_tag": "en"}, auth)
gid = group["id"]
gpath = "/v2/group/" + gid

def group_states():
    return {u["user"]["id"]: u.get("state", 0) for u in request("GET", gpath + "/user?limit=100", auth=auth).get("group_users", [])}

request("POST", gpath + "/join", auth=custom_auth)
assert group_states() == {owner: 0, custom_owner: 3}
assert notification_count(auth, -5) == 1
request("POST", gpath + "/add?user_ids=" + email_owner, auth=custom_auth, expected=404)
request("POST", gpath + "/add?user_ids=" + custom_owner, auth=auth)
assert group_states()[custom_owner] == 2
assert notification_count(custom_auth, -4) == 1
request("POST", gpath + "/promote?user_ids=" + custom_owner, auth=auth)
assert group_states()[custom_owner] == 1
request("PUT", gpath, {"open": True, "description": "Local فارسی group"}, custom_auth)
request("POST", gpath + "/join", auth=email_auth)
assert group_states()[email_owner] == 2
request("POST", gpath + "/leave", auth=auth, expected=400)
request("POST", gpath + "/demote?user_ids=" + custom_owner, auth=auth)
assert group_states()[custom_owner] == 2
request("POST", gpath + "/ban?user_ids=" + email_owner, auth=auth)
assert email_owner not in group_states()
bans = request("GET", gpath + "/user?state=4", auth=auth)["group_users"]
assert bans[0]["user"]["id"] == email_owner
assert not request("GET", "/v2/user/" + email_owner + "/group", auth=email_auth).get("user_groups")
request("POST", gpath + "/kick?user_ids=" + email_owner, auth=auth)
request("POST", gpath + "/join", auth=email_auth)
first = request("GET", gpath + "/user?limit=1", auth=auth)
seen = {first["group_users"][0]["user"]["id"]}
while first.get("cursor"):
    first = request("GET", gpath + "/user?limit=1&cursor=" + urllib.parse.quote(first["cursor"]), auth=auth)
    seen.update(u["user"]["id"] for u in first.get("group_users", []))
assert seen == {owner, custom_owner, email_owner}
found = request("GET", "/v2/group?name=" + urllib.parse.quote(group_name.upper()), auth=auth)["groups"]
assert [g["id"] for g in found] == [gid]
assert found[0]["edge_count"] == 3
request("POST", gpath + "/leave", auth=email_auth)
request("DELETE", gpath, auth=auth)
assert not request("GET", "/v2/user/" + custom_owner + "/group", auth=custom_auth).get("user_groups")
assert not request("GET", gpath + "/user", auth=auth).get("group_users")
print("PASS group create/update/delete, requests/acceptance, roles, bans/unban, last-owner protection and pagination")

value = '{"nested":{"text":"فارسی 🂡","score":7}}'
write = {"collection": "port-smoke", "key": "round", "value": value, "version": "*", "permission_read": 1, "permission_write": 1}
acks = request("PUT", "/v2/storage", {"objects": [write]}, auth)
version = acks["acks"][0]["version"]
assert version == hashlib.md5(value.encode()).hexdigest()
ids = {"object_ids": [{"collection": "port-smoke", "key": "round", "user_id": owner}]}
read = request("POST", "/v2/storage", ids, auth)
assert json.loads(read["objects"][0]["value"]) == json.loads(value)
listed = request("GET", "/v2/storage/port-smoke?user_id=" + urllib.parse.quote(owner), auth=auth)
assert len(listed["objects"]) == 1
write["version"] = "stale"
request("PUT", "/v2/storage", {"objects": [write]}, auth, expected=400)
request("PUT", "/v2/storage/delete", {"object_ids": [{"collection": "port-smoke", "key": "round"}]}, auth)
assert not request("POST", "/v2/storage", ids, auth).get("objects")
request("GET", "/v2/notification", auth=auth)
print("PASS JSON storage/version hashing, list/read/delete, stale-write rejection, notification list")
# Public user lookup shares the new native bulk query path.
users = request("GET", "/v2/user?ids=" + owner.upper() + "&usernames=" + email_username, auth=auth)["users"]
assert {user["id"] for user in users} == {owner, email_owner}
assert not request("GET", "/v2/user?usernames=" + urllib.parse.quote("\" OR 1=1 --"), auth=auth).get("users")

# Keep a real storage object through deletion to verify the database cascade.
write["version"] = "*"
request("PUT", "/v2/storage", {"objects": [write]}, auth)
request("DELETE", "/v2/account", auth=auth)
request("GET", "/v2/account", auth=auth, expected=(401, 404))
request("POST", "/v2/account/session/refresh", {"token": session["refresh_token"]}, expected=(401, 404))
request("POST", "/v2/account/authenticate/device?create=false", {"id": device}, expected=404)
assert not request("POST", "/v2/storage", ids, "Bearer " + custom_session["token"]).get("objects")
assert not request("GET", "/v2/user?ids=" + owner, auth="Bearer " + custom_session["token"]).get("users")
for test_session in (custom_session, email_session):
    request("DELETE", "/v2/account", auth="Bearer " + test_session["token"])
print("PASS user lookup, account deletion, storage cascade, deleted login/session rejection")
print("Accounts created by this HTTP smoke were deleted.")
