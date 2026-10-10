local nk = require("nakama")
local function rank_contract(ctx, payload)
  assert(ctx.user_id and ctx.user_id ~= "", "authenticated test account required")
  local account = nk.account_get_id(ctx.user_id)
  assert(account.devices and #account.devices == 1 and string.sub(account.devices[1].id, 1, 16) == "local-rank-test-", "test account required")
  local id = "local_rank_" .. ctx.user_id
  if payload == "cleanup" then
    nk.leaderboard_delete(id)
    nk.leaderboard_delete(id .. "_auth")
    nk.tournament_delete(id .. "_tour")
    return nk.json_encode({ok=true})
  end
  if payload == "attempt" then
    nk.tournament_add_attempt(id .. "_tour", ctx.user_id, 1)
    return nk.json_encode({ok=true})
  end
  nk.leaderboard_create(id, false, "desc", "best", nil, {source="runtime"}, true)
  nk.leaderboard_create(id .. "_auth", true, "asc", "set", nil, nil, true)
  local record = nk.leaderboard_record_write(id .. "_auth", ctx.user_id, ctx.username, 7, 1, {source="authoritative"})
  assert(record.score == 7)
  nk.tournament_create(id .. "_tour", false, "desc", "set", 3600, nil, {source="runtime"}, "Local tournament", "contract", 1, 0, 0, 2, 2, true, true)
  return nk.json_encode({leaderboard=id, authoritative=id .. "_auth", tournament=id .. "_tour"})
end
nk.register_rpc(rank_contract, "local_rank_contract")
