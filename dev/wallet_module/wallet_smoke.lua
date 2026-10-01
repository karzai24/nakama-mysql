-- Local-only runtime contract. Never copied into the final server image.
local nk = require("nakama")
local function wallet_smoke(ctx, _payload)
  assert(ctx.user_id and ctx.user_id ~= "", "authenticated test account required")
  local uid = ctx.user_id
  local account = nk.account_get_id(uid)
  assert(account.devices and #account.devices == 1, "fresh device account required")
  assert(string.sub(account.devices[1].id, 1, 18) == "local-wallet-test-", "test account required")
  assert(next(account.wallet) == nil, "fresh wallet required")
  local current, previous = nk.wallet_update(uid, {coins=10}, {source="runtime"}, true)
  assert(current.coins == 10 and previous.coins == nil)
  current, previous = nk.wallet_update(uid, {coins=-2}, nil, false)
  assert(current.coins == 8 and previous.coins == 10)
  local entries = nk.wallet_ledger_list(uid, 100)
  assert(#entries == 1 and entries[1].changeset.coins == 10)
  nk.wallet_ledger_update(entries[1].id, {checked=true})
  entries = nk.wallet_ledger_list(uid, 100)
  assert(entries[1].metadata.checked and entries[1].metadata.source == "runtime")
  nk.wallets_update({{user_id=uid, changeset={coins=2}, metadata={source="batch"}}}, true)
  nk.storage_write({{collection="wallet_smoke", key="remove", user_id=uid, value={value=1}}})
  local acks, wallets = nk.multi_update(
    {{user_id=uid, display_name="wallet committed"}},
    {{collection="wallet_smoke", key="reward", user_id=uid, value={value=2}}},
    {{collection="wallet_smoke", key="remove", user_id=uid}},
    {{user_id=uid, changeset={coins=-5}, metadata={source="multi"}}}, true)
  assert(#acks == 1 and #wallets == 1 and wallets[1].updated.coins == 5)
  local ok = pcall(nk.multi_update,
    {{user_id=uid, display_name="must roll back"}},
    {{collection="wallet_smoke", key="rejected", user_id=uid, value={value=3}}},
    {{collection="wallet_smoke", key="reward", user_id=uid}},
    {{user_id=uid, changeset={coins=-6}}}, true)
  assert(not ok, "overspend must fail")
  account = nk.account_get_id(uid)
  assert(account.wallet.coins == 5 and account.user.display_name == "wallet committed")
  local objects = nk.storage_read({
    {collection="wallet_smoke",key="reward",user_id=uid},
    {collection="wallet_smoke",key="rejected",user_id=uid},
    {collection="wallet_smoke",key="remove",user_id=uid}})
  assert(#objects == 1 and objects[1].key == "reward")
  entries = nk.wallet_ledger_list(uid, 100)
  assert(#entries == 3, "failed multi update must not leave a ledger row")
  local page, cursor = nk.wallet_ledger_list(uid, 1)
  assert(#page == 1 and cursor ~= "")
  local second = nk.wallet_ledger_list(uid, 1, cursor)
  assert(#second == 1 and page[1].id ~= second[1].id)
  return nk.json_encode({ok=true})
end
nk.register_rpc(wallet_smoke, "local_wallet_contract")
