#!/usr/bin/env python3
"""Local console HTTP contracts. Secrets/tokens stay in memory; fixtures are scoped."""
import base64
import csv
import io
import hashlib
import hmac
import json
import pathlib
import re
import struct
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

root = pathlib.Path(__file__).resolve().parents[1]
env = {}
for line in (root / '.env').read_text().splitlines():
    if line and not line.startswith('#') and '=' in line:
        k, v = line.split('=', 1)
        env[k] = v
backend = sys.argv[1] if len(sys.argv) > 1 else 'mysql'
assert backend == 'mysql'
base = 'http://127.0.0.1:7351/v2/console'

def request(method, path, data=None, token=None, expected=200, game=False, raw_body=None, content_type="application/json"):
    url = ('http://127.0.0.1:7350' if game else base) + path
    headers = {'Content-Type': content_type}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    elif game:
        headers['Authorization'] = 'Basic ' + base64.b64encode((env['NAKAMA_SERVER_KEY'] + ':').encode()).decode()
    body = raw_body if raw_body is not None else (json.dumps(data).encode() if data is not None else None)
    req = urllib.request.Request(url, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            code, raw = response.status, response.read()
    except urllib.error.HTTPError as error:
        code, raw = error.code, error.read()
    assert code == expected, f'{method} {urllib.parse.urlsplit(path).path}: expected {expected}, got {code}'
    return json.loads(raw) if raw and raw_body is None else {}

def upload_storage(extension, payload, token, expected=200):
    boundary = 'console-' + uuid.uuid4().hex
    body = (f'--{boundary}\r\nContent-Disposition: form-data; name="fixture.{extension}"; filename="fixture.{extension}"\r\nContent-Type: application/octet-stream\r\n\r\n'.encode()
            + payload.encode() + f'\r\n--{boundary}--\r\n'.encode())
    return request('POST', '/storage/import', token=token, expected=expected,
                   raw_body=body, content_type='multipart/form-data; boundary=' + boundary)

def login(name, password, mfa=''):
    return request('POST', '/authenticate', {'username': name, 'password': password, 'mfa': mfa})

def totp(secret):
    key = base64.b32decode(secret + '=' * (-len(secret) % 8))
    digest = hmac.new(key, struct.pack('>Q', int(time.time()) // 30), hashlib.sha1).digest()
    offset = digest[-1] & 15
    return f'{(struct.unpack(">I", digest[offset:offset+4])[0] & 0x7fffffff) % 1000000:06d}'

def claims(token):
    payload = token.split('.')[1]
    return json.loads(base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4)))

admin = login('localadmin', env['NAKAMA_CONSOLE_PASSWORD'])['token']
name = 'console' + uuid.uuid4().hex[:10]
password = uuid.uuid4().hex
resources = (root / 'console/console.proto').read_text().split('enum AclResources {', 1)[1].split('}', 1)[0]
permissions = {n: {'read': True, 'write': True, 'delete': True} for n in re.findall(r'^\s*(\w+)\s*=', resources, re.M)}
account_ids, group_ids, template_ids = [], [], []
user_created = False
try:
    request('GET', '/user', token=None, expected=401)
    added = request('POST', '/user', {'username': name, 'email': name + '@example.invalid', 'acl': permissions, 'newsletter_subscription': False, 'mfa_required': True}, admin)
    user_created = True
    request('POST', '/authenticate/password_change', {'code': added['token'], 'password': password})
    challenge = login(name, password)
    assert not challenge.get('token') and challenge.get('mfa_code')
    secret = claims(challenge['mfa_code'])['secret']
    enrolled = request('POST', '/authenticate/mfa', {'mfa': challenge['mfa_code'], 'code': totp(secret)})
    actor = login(name, password, totp(secret))['token']
    request('GET', '/user/' + name, token=actor)
    request('POST', '/authenticate', {'username': name, 'password': password}, expected=403)
    recovery = enrolled['recovery_codes'][0]
    login(name, password, recovery)
    request('POST', '/authenticate', {'username': name, 'password': password, 'mfa': recovery}, expected=401)
    print('PASS console login, invitation, MFA enrollment, TOTP and single-use recovery')

    for suffix in ('one', 'two'):
        game_session = request('POST', '/v2/account/authenticate/device?create=true', {'id': 'console-smoke-' + uuid.uuid4().hex}, game=True)
        uid = claims(game_session['token'])['uid']
        account_ids.append(uid)
        if suffix == 'one':
            game_token = game_session['token']
    owner, member = account_ids
    account_password = uuid.uuid4().hex
    request('POST', '/account/' + owner, {'display_name': 'Console فارسی', 'metadata': '{"tested":true}', 'wallet': '{"coins":11}', 'email': name + '@example.invalid', 'password': account_password}, actor)
    found = request('GET', '/account?' + urllib.parse.urlencode({'filter': owner}), token=actor)
    assert found['users'][0]['id'] == owner
    collection = 'console_' + uuid.uuid4().hex
    request('PUT', f'/storage/{collection}/item/{owner}', {'value': '{"v":1}', 'permission_read': 1, 'permission_write': 1}, actor)
    obj = request('GET', f'/storage/{collection}/item/{owner}', token=actor)
    assert json.loads(obj['value']) == {'v': 1}
    listed = request('GET', '/storage?' + urllib.parse.urlencode({'collection': collection, 'user_id': owner}), token=actor)
    assert len(listed['objects']) == 1
    imported = {'collection': collection, 'key': 'json', 'user_id': owner, 'value': {'imported': True}, 'permission_read': 1, 'permission_write': 1}
    upload_storage('json', json.dumps([imported]), actor)
    csv_file = io.StringIO()
    writer = csv.writer(csv_file)
    writer.writerow(['collection', 'key', 'user_id', 'value', 'permission_read', 'permission_write'])
    writer.writerow([collection, 'csv', owner, '{"imported":true}', 1, 1])
    upload_storage('csv', csv_file.getvalue(), actor)
    assert len(request('GET', '/storage?' + urllib.parse.urlencode({'collection': collection, 'user_id': owner}), token=actor)['objects']) == 3
    note = request('POST', f'/account/{owner}/note', {'note': 'Console contract'}, actor)
    assert note['id']
    assert len(request('GET', f'/account/{owner}/note', token=actor)['notes']) == 1
    exported = request('GET', f'/account/{owner}/export', token=actor)
    request('PUT', f'/account/{owner}/import', {'data': exported}, actor)
    print('PASS console account edit/search, JSON/CSV storage import, notes, account export/import')

    group = request('POST', '/v2/group', {'name': 'console_' + uuid.uuid4().hex, 'open': False, 'max_count': 4}, game_token, game=True)
    gid = group['id']; group_ids.append(gid)
    request('POST', f'/group/{gid}/add', {'ids': member, 'join_request': True}, actor)
    request('POST', f'/group/{gid}/account/{member}/promote', {}, actor)
    assert request('GET', '/group?filter=' + gid, token=actor)['groups'][0]['edge_count'] == 2
    request('POST', f'/group/{gid}/account/{member}/promote', {}, actor)
    request('POST', f'/group/{gid}/account/{owner}/demote', {}, actor, expected=400)
    request('POST', '/notification', {'user_ids': [owner], 'subject': 'Console test', 'content': {'ok': True}, 'code': 1, 'persistent': True}, actor)
    notification = request('GET', '/notification?limit=10&user_id=' + owner, token=actor)['notifications'][0]
    request('GET', '/notification/' + notification['id'], token=actor)
    request('DELETE', '/notification/' + notification['id'], token=actor)
    for path in ('/purchase?limit=10&user_id=', '/subscription?limit=10&user_id='):
        request('GET', path + owner, token=actor)
    print('PASS console group requests/roles, notifications and billing history screens')

    template = request('POST', '/acl/template', {'name': name, 'description': 'test', 'acl': {'ACCOUNT': {'read': True}}}, actor)
    template_ids.append(template['id'])
    request('PUT', '/acl/template/' + template['id'], {'name': name, 'description': 'updated', 'acl': {'ACCOUNT': {'read': True}}}, actor)
    request('GET', '/acl/template', token=actor)
    setting = request('GET', '/setting/utc_toggle', token=actor)
    request('POST', '/setting/utc_toggle', {'value': setting['value']}, actor)
    request('GET', '/setting?names=utc_toggle', token=actor)
    cfg = request('GET', '/config', token=actor)
    assert env['MYSQL_PASSWORD'] not in cfg['config'] and env['NAKAMA_CONSOLE_PASSWORD'] not in cfg['config']
    audits = request('GET', '/audit/log?' + urllib.parse.urlencode({'username': name, 'limit': 2}), token=admin)
    assert audits['entries'] and audits['next_cursor']
    all_audits = request('GET', '/audit/log?' + urllib.parse.urlencode({'username': name, 'limit': 100}), token=admin)['entries']
    assert any('[REDACTED]' in entry['metadata'] for entry in all_audits)
    assert account_password not in json.dumps(all_audits)
    # Both engines must preserve audit filters across pages.
    next_page = request('GET', '/audit/log?' + urllib.parse.urlencode({'username': name, 'limit': 2, 'cursor': audits['next_cursor']}), token=admin)
    assert next_page['entries']
    request('GET', '/api/endpoints', token=actor)
    request('GET', '/status', token=actor)
    print('PASS console ACL templates, settings, redacted config, audits, explorer and status')

    # Exercise route ACLs with a non-admin, including audit username options and templates.
    request('PUT', '/user/' + name, {'acl': {'AUDIT_LOG': {'read': True}, 'ACL_TEMPLATE': {'read': True}, 'NOTIFICATION': {'write': True}}}, admin)
    scoped = login(name, password, totp(secret))['token']
    request('GET', '/audit/log/users', token=scoped)
    request('GET', '/acl/template', token=scoped)
    request('POST', '/notification', {'user_ids': [owner], 'subject': 'Scoped console', 'content': {'ok': True}, 'code': 1, 'persistent': True}, scoped)
    request('DELETE', '/account/' + owner, token=scoped, expected=403)
    request('PUT', '/user/' + name, {'acl': {'ACCOUNT': {'read': True}}}, admin)
    request('GET', '/user', token=actor, expected=401)
    upload_storage('json', '[]', actor, expected=401)
    restricted = login(name, password, totp(secret))['token']
    request('GET', '/account?filter=' + owner, token=restricted)
    request('DELETE', '/account/' + owner, token=restricted, expected=403)
    # Verify old privileged JWTs stay invalid after recreating Nakama.
    subprocess.run(['docker', 'compose', '-f', 'compose.dev.yml', 'restart', 'nakama'], cwd=root, check=True, stdout=subprocess.DEVNULL)
    for _ in range(60):
        try:
            admin = login('localadmin', env['NAKAMA_CONSOLE_PASSWORD'])['token']
            break
        except (OSError, AssertionError):
            time.sleep(0.5)
    else:
        raise AssertionError('Console did not recover after restart')
    request('GET', '/user', token=actor, expected=401)
    upload_storage('json', '[]', actor, expected=401)
    request('GET', '/account?filter=' + owner, token=restricted)
    reset = request('POST', '/user/' + name + '/reset/password', {}, admin)
    request('POST', '/authenticate/password_change', {'code': reset['code'], 'password': password})
    request('POST', '/authenticate/password_change', {'code': reset['code'], 'password': password}, expected=401)
    print('PASS console permission enforcement, single-use reset and restart revocation')
finally:
    # Refresh root credentials if a failed test interrupted a restart.
    admin = login('localadmin', env['NAKAMA_CONSOLE_PASSWORD'])['token']
    for gid in group_ids:
        request('DELETE', '/group/' + gid, token=admin)
    for uid in account_ids:
        request('DELETE', '/account/' + uid, token=admin)
    for tid in template_ids:
        request('DELETE', '/acl/template/' + tid, token=admin)
    if user_created:
        request('DELETE', '/user?username=' + name, token=admin)
print('PASS scoped console fixtures removed; audit records retained')
