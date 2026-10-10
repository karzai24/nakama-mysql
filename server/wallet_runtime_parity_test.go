// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"testing"

	"github.com/dop251/goja"
	"github.com/heroiclabs/nakama-common/runtime"
	lua "github.com/heroiclabs/nakama/v3/internal/gopher-lua"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Call the actual bindings, not just their factories: wrapper conversions and
// returned ledger fields are part of the database contract presented to scripts.
func TestDualBackendWalletRuntimeContracts(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	t.Run("go", func(t *testing.T) {
		owner, _ := lifecycleUser(t, db)
		n := &RuntimeGoNakamaModule{db: db, logger: zap.NewNop()}
		results, err := n.WalletsUpdate(ctx, []*runtime.WalletUpdate{
			{UserID: owner.String(), Changeset: map[string]int64{"coins": 5}, Metadata: map[string]interface{}{"keep": true}},
			{UserID: owner.String(), Changeset: map[string]int64{"coins": -2}, Metadata: map[string]interface{}{"keep": true}},
		}, true)
		require.NoError(t, err)
		require.Len(t, results, 2)
		require.Equal(t, int64(5), results[0].Updated["coins"])
		require.Equal(t, int64(5), results[1].Previous["coins"])
		require.Equal(t, int64(3), results[1].Updated["coins"])
		first, next, err := n.WalletLedgerList(ctx, owner.String(), 1, "")
		require.NoError(t, err)
		require.Len(t, first, 1)
		require.NotEmpty(t, next)
		second, end, err := n.WalletLedgerList(ctx, owner.String(), 1, next)
		require.NoError(t, err)
		require.Len(t, second, 1)
		require.Empty(t, end)
		require.NotEqual(t, first[0].GetID(), second[0].GetID())
		item, err := n.WalletLedgerUpdate(ctx, first[0].GetID(), map[string]interface{}{"text": "فارسی 🎲"})
		require.NoError(t, err)
		require.Equal(t, first[0].GetID(), item.GetID())
		require.Equal(t, owner.String(), item.GetUserID())
		require.Equal(t, first[0].GetChangeset(), item.GetChangeset())
		require.Equal(t, map[string]interface{}{"keep": true, "text": "فارسی 🎲"}, item.GetMetadata())
		updated, previous, err := n.WalletUpdate(ctx, owner.String(), map[string]int64{"coins": -4}, nil, true)
		var negative *runtime.WalletNegativeError
		require.ErrorAs(t, err, &negative)
		require.Nil(t, updated)
		require.Nil(t, previous)
		for _, limit := range []int{-1, 101} {
			_, _, err = n.WalletLedgerList(ctx, owner.String(), limit, "")
			require.Error(t, err)
		}
		empty, cursor, err := n.WalletLedgerList(ctx, owner.String(), 0, "")
		require.NoError(t, err)
		require.Empty(t, empty)
		require.Empty(t, cursor)
		_, _, err = n.WalletLedgerList(ctx, owner.String(), 1, "bad-cursor")
		require.ErrorIs(t, err, runtime.ErrWalletLedgerInvalidCursor)
		require.Equal(t, int64(3), walletBalance(t, db, owner)["coins"])
		require.Equal(t, 2, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", owner))
	})
	t.Run("javascript", func(t *testing.T) {
		owner, _ := lifecycleUser(t, db)
		r := goja.New()
		n := &RuntimeJavascriptNakamaModule{ctx: ctx, db: db, logger: zap.NewNop(), config: NewConfig(zap.NewNop())}
		obj := r.NewObject()
		for name, fn := range n.mappings(r) {
			require.NoError(t, obj.Set(name, fn))
		}
		require.NoError(t, r.Set("nk", obj))
		require.NoError(t, r.Set("uid", owner.String()))
		_, err := r.RunString(`
        function check(value, name) { if (!value) throw new Error(name); }
        function rejected(fn) { var caught=false;try { fn(); } catch(e) { caught=true; } check(caught,"expected rejection"); }
        var results=nk.walletsUpdate([
            {userId:uid,changeset:{coins:5},metadata:{keep:true}},
            {userId:uid,changeset:{coins:-2},metadata:{keep:true}}
        ],true);
        check(results.length===2 && results[0].updated.coins===5 && results[1].previous.coins===5 && results[1].updated.coins===3,"snapshots");
        var first=nk.walletLedgerList(uid,1);
        var second=nk.walletLedgerList(uid,1,first.cursor);
        check(first.items.length===1 && second.items.length===1 && first.cursor && second.cursor===null,"pagination");
        check(first.items[0].id!==second.items[0].id,"no duplicate rows");
        var item=nk.walletLedgerUpdate(first.items[0].id,{text:"فارسی 🎲"});
        check(item.id===first.items[0].id && item.userId===uid,"ledger identity");
        check(item.changeset.coins===first.items[0].changeset.coins && item.changeset.text===undefined,"original changeset");
        check(item.metadata.keep===true && item.metadata.text==="فارسی 🎲","merged metadata");
        rejected(function(){nk.walletUpdate(uid,{coins:-4},{},true)});
        rejected(function(){nk.walletsUpdate([{userId:uid,changeset:{coins:1.5}}],true)});
        rejected(function(){nk.walletLedgerList(uid,-1)});
        rejected(function(){nk.walletLedgerList(uid,101)});
        rejected(function(){nk.walletLedgerList(uid,1,"bad-cursor")});
        var empty=nk.walletLedgerList(uid,0);check(empty.items.length===0 && empty.cursor===null,"zero limit");
        check(nk.accountGetId(uid).wallet.coins===3 && nk.walletLedgerList(uid).items.length===2,"rollback");
        `)
		require.NoError(t, err)
	})
	t.Run("lua", func(t *testing.T) {
		owner, _ := lifecycleUser(t, db)
		l := lua.NewState()
		defer l.Close()
		l.SetContext(ctx)
		n := &RuntimeLuaNakamaModule{db: db, logger: zap.NewNop(), config: NewConfig(zap.NewNop())}
		l.PreloadModule("nakama", n.Loader)
		l.SetGlobal("uid", lua.LString(owner.String()))
		require.NoError(t, l.DoString(`
        local nk=require("nakama")
        local results=nk.wallets_update({
            {user_id=uid,changeset={coins=5},metadata={keep=true}},
            {user_id=uid,changeset={coins=-2},metadata={keep=true}}
        },true)
        assert(#results==2 and results[1].updated.coins==5 and results[2].previous.coins==5 and results[2].updated.coins==3,"snapshots")
        local first,next=nk.wallet_ledger_list(uid,1)
        local second,last=nk.wallet_ledger_list(uid,1,next)
        assert(#first==1 and #second==1 and next~="" and last=="","pagination")
        assert(first[1].id~=second[1].id,"no duplicate rows")
        local item=nk.wallet_ledger_update(first[1].id,{text="فارسی 🎲"})
        assert(item.id==first[1].id and item.user_id==uid,"ledger identity")
        assert(item.changeset.coins==first[1].changeset.coins,"original changeset")
        assert(item.metadata.keep==true and item.metadata.text=="فارسی 🎲","merged metadata")
        assert(not pcall(nk.wallet_update,uid,{coins=-4},{},true))
        assert(not pcall(nk.wallets_update,{{user_id=uid,changeset={coins=1.5}}},true))
        assert(not pcall(nk.wallet_ledger_list,uid,-1))
        assert(not pcall(nk.wallet_ledger_list,uid,101))
        assert(not pcall(nk.wallet_ledger_list,uid,1,"bad-cursor"))
        local empty,cursor=nk.wallet_ledger_list(uid,0);assert(#empty==0 and cursor=="","zero limit")
        assert(nk.account_get_id(uid).wallet.coins==3 and #nk.wallet_ledger_list(uid)==2,"rollback")
        `))
	})
}
