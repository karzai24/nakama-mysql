// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"testing"

	"github.com/dop251/goja"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/runtime"
	lua "github.com/heroiclabs/nakama/v3/internal/gopher-lua"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The scripts run through the real public runtime bindings and real databases.
// SQL remains native to the selected engine; result shape and domain behavior
// are the cross-backend contract, not SQL source compatibility.
func TestDualBackendRuntimeSQLAndAdapters(t *testing.T) {
	db := identityTestDB(t)
	ctx := context.Background()
	owner, _ := lifecycleUser(t, db)
	selectSQL := `SELECT username, metadata, display_name, disable_time, NULL AS absent, 42 AS amount FROM users WHERE id = ?`
	updateSQL := `UPDATE users SET display_name = ? WHERE id = ?`
	rawSQL := `SELECT UNHEX('0041ff') AS bytes, CAST(12.50 AS DECIMAL(8,2)) AS exact_decimal`

	pID := "runtime_" + GenerateString()
	sID := "runtime_" + GenerateString()
	t.Cleanup(func() {
		lifecycleExec(t, db, "DELETE FROM purchase WHERE transaction_id = ?", pID)
		lifecycleExec(t, db, "DELETE FROM subscription WHERE original_transaction_id = ?", sID)
	})
	_, err := upsertPurchases(ctx, db, []*storagePurchase{billingPurchase(owner, pID)})
	require.NoError(t, err)
	require.NoError(t, persistSubscriptions(ctx, db, []*storageSubscription{billingSubscription(owner, sID)}))
	cfg := NewConfig(zap.NewNop())
	r := goja.New()
	n := &RuntimeJavascriptNakamaModule{ctx: ctx, db: db, logger: zap.NewNop(), config: cfg, metrics: metrics, storageIndex: &walletTestIndex{}}
	obj := r.NewObject()
	for name, fn := range n.mappings(r) {
		require.NoError(t, obj.Set(name, fn))
	}
	require.NoError(t, r.Set("nk", obj))
	for k, v := range map[string]any{"uid": owner.String(), "selectSQL": selectSQL, "updateSQL": updateSQL, "rawSQL": rawSQL, "pid": pID} {
		require.NoError(t, r.Set(k, v))
	}
	_, err = r.RunString(`
 var checkNumber=0; function check(v) { checkNumber++; if (!v) throw new Error("runtime parity assertion "+checkNumber); }
 check(nk.sqlExec(updateSQL,["متن 🎲",uid]).rowsAffected===1);
 check(nk.sqlExec(updateSQL,["متن 🎲",uid]).rowsAffected===1); // Unchanged UPDATE counts a match.
 var row=nk.sqlQuery(selectSQL,[uid])[0];
 check(typeof row.username==="string" && typeof row.metadata==="object");
 check(JSON.parse(String.fromCharCode.apply(null,row.metadata)) && row.display_name==="متن 🎲" && row.absent===null && row.amount===42);
 var raw=nk.sqlQuery(rawSQL)[0]; check(raw.bytes[0]===0 && raw.bytes[1]===65 && raw.bytes[2]===255);
 check(String(raw.exact_decimal)==="12.50");
 check(nk.sqlQuery(selectSQL,["00000000-0000-0000-0000-ffffffffffff"]).length===0);
 var caught=false;try{nk.sqlQuery("SELECT * FROM no_such_parity_table");}catch(e){caught=true;}check(caught);
 var writes=nk.storageWrite([{collection:"runtime_parity",key:"js",userId:uid,value:{text:"متن",n:42},version:"*"}]);check(writes.length===1);
 check(nk.storageRead([{collection:"runtime_parity",key:"js",userId:uid}])[0].value.n===42);
 caught=false;try{nk.storageWrite([{collection:"runtime_parity",key:"js",userId:uid,value:{},version:"stale"}]);}catch(e){caught=true;}check(caught);
 check(nk.walletUpdate(uid,{coins:7},{source:"js"},true).updated.coins===7);
 check(nk.accountGetId(uid).wallet.coins===7);
 check(nk.purchaseGetByTransactionId(pid).transactionId===pid);
 check(nk.subscriptionGetByProductId(uid,"monthly").active===true);
 `)
	require.NoError(t, err)
	l := lua.NewState()
	defer l.Close()
	l.SetContext(ctx)
	ln := &RuntimeLuaNakamaModule{db: db, logger: zap.NewNop(), config: cfg, metrics: metrics, storageIndex: &walletTestIndex{}}
	l.PreloadModule("nakama", ln.Loader)
	for k, v := range map[string]string{"uid": owner.String(), "select_sql": selectSQL, "update_sql": updateSQL, "raw_sql": rawSQL, "pid": pID} {
		l.SetGlobal(k, lua.LString(v))
	}
	require.NoError(t, l.DoString(`
 local nk=require("nakama")
 assert(nk.sql_exec(update_sql,{"Lua متن",uid})==1)
 assert(nk.sql_exec(update_sql,{"Lua متن",uid})==1)
 local row=nk.sql_query(select_sql,{uid})[1]
 assert(type(row.username)=="string" and type(row.metadata)=="string" and row.display_name=="Lua متن")
 assert(row.absent==nil and row.amount==42 and type(row.disable_time)=="number")
 local raw=nk.sql_query(raw_sql)[1];assert(raw.bytes==string.char(0,65,255) and tostring(raw.exact_decimal)=="12.50")
 assert(#nk.sql_query(select_sql,{"00000000-0000-0000-0000-ffffffffffff"})==0)
 assert(not pcall(nk.sql_query,"SELECT * FROM no_such_parity_table"))
 local acks=nk.storage_write({{collection="runtime_parity",key="lua",user_id=uid,value={text="متن",n=43},version="*"}})
 assert(#acks==1 and nk.storage_read({{collection="runtime_parity",key="lua",user_id=uid}})[1].value.n==43)
 local updated,previous=nk.wallet_update(uid,{coins=2},{source="lua"},true);assert(updated.coins==9 and previous.coins==7)
 assert(nk.account_get_id(uid).wallet.coins==9)
 assert(nk.purchase_get_by_transaction_id(pid).transaction_id==pid)
 assert(nk.subscription_get_by_product_id(uid,"monthly").active)
 `))
	gn := &RuntimeGoNakamaModule{db: db, logger: zap.NewNop(), config: cfg, metrics: metrics, storageIndex: &walletTestIndex{}}
	acks, err := gn.StorageWrite(ctx, []*runtime.StorageWrite{{UserID: owner.String(), Collection: "runtime_parity", Key: "go", Value: `{"n":44}`, Version: "*"}})
	require.NoError(t, err)
	require.Len(t, acks, 1)
	p, err := gn.PurchaseGetByTransactionId(ctx, pID)
	require.NoError(t, err)
	require.Equal(t, pID, p.TransactionId)
	sub, err := gn.SubscriptionGetByProductId(ctx, owner.String(), "monthly")
	require.NoError(t, err)
	require.True(t, sub.Active)
	// Raw Go SQL intentionally uses the selected driver's native values.
	displaySQL := "SELECT display_name FROM users WHERE id = $1"
	if DbIsMySQL(db) {
		displaySQL = "SELECT display_name FROM users WHERE id = ?"
	}
	var display string
	require.NoError(t, db.QueryRowContext(ctx, displaySQL, owner).Scan(&display))
	require.Equal(t, "Lua متن", display)
}

func TestDualBackendStatusFollowLookup(t *testing.T) {
	db := identityTestDB(t)
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	var name string
	require.NoError(t, db.QueryRow(lifecycleSQL(db, "SELECT username FROM users WHERE id = ?"), b).Scan(&name))
	rows, err := statusFollowUsersMySQL(context.Background(), db, map[uuid.UUID]struct{}{a: {}, uuid.Must(uuid.NewV4()): {}}, map[string]struct{}{name: {}, "missing_name": {}})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Contains(t, rows, a)
	require.Equal(t, name, rows[b])
	rows, err = statusFollowUsersMySQL(context.Background(), db, nil, nil)
	require.NoError(t, err)
	require.Empty(t, rows)
}
