// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Observe index publication and read through another DB connection in callbacks:
// visibility there proves the database transaction committed before publication.
type walletTestIndex struct {
	StorageIndex
	writes  []*api.StorageObject
	deletes StorageOpDeletes
	check   func()
}

func (s *walletTestIndex) Write(_ context.Context, objects []*api.StorageObject) (int, int) {
	if len(objects) > 0 && s.check != nil {
		s.check()
	}
	s.writes = append(s.writes, objects...)
	return len(objects), 0
}
func (s *walletTestIndex) Delete(_ context.Context, objects StorageOpDeletes) int {
	if len(objects) > 0 && s.check != nil {
		s.check()
	}
	s.deletes = append(s.deletes, objects...)
	return len(objects)
}
func walletStorageWrite(user uuid.UUID, key, value, version string) *StorageOpWrite {
	return &StorageOpWrite{OwnerID: user.String(), Object: &api.WriteStorageObject{Collection: "wallet_contract", Key: key, Value: value, Version: version, PermissionRead: wrapperspb.Int32(1), PermissionWrite: wrapperspb.Int32(1)}}
}
func walletStorageDelete(user uuid.UUID, key, version string) *StorageOpDelete {
	return &StorageOpDelete{OwnerID: user.String(), ObjectID: &api.DeleteStorageObjectId{Collection: "wallet_contract", Key: key, Version: version}}
}

func TestDualBackendMultiUpdateAtomicity(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	for _, failure := range []string{"success", "account", "write", "delete", "wallet", "ledger"} {
		t.Run(failure, func(t *testing.T) {
			user, _ := lifecycleUser(t, db)
			other, _ := lifecycleUser(t, db)
			_, _, err := StorageWriteObjects(ctx, logger, db, metrics, &walletTestIndex{}, true, StorageOpWrites{walletStorageWrite(user, "old", `{"before":true}`, "")})
			require.NoError(t, err)
			lifecycleExec(t, db, "UPDATE users SET display_name = ?,wallet = ? WHERE id = ?", "before", `{"coins":10}`, user)
			accounts := []*accountUpdate{{userID: user, displayName: wrapperspb.String("after")}}
			writes := StorageOpWrites{walletStorageWrite(user, "z", `{"value":2}`, "*"), walletStorageWrite(user, "a", `{"value":1}`, "*")}
			deletes := StorageOpDeletes{walletStorageDelete(user, "old", "")}
			wallets := []*walletUpdate{{UserID: user, Changeset: map[string]int64{"coins": -3}, Metadata: `{"source":"multi"}`}}
			switch failure {
			case "account":
				var name string
				require.NoError(t, db.QueryRow(lifecycleSQL(db, "SELECT username FROM users WHERE id = ?"), other).Scan(&name))
				accounts[0].username = name
			case "write":
				writes = append(writes, walletStorageWrite(user, "old", `{}`, "*"))
			case "delete":
				deletes[0].ObjectID.Version = "stale"
			case "wallet":
				wallets[0].Changeset["coins"] = -11
			case "ledger":
				wallets[0].Metadata = "invalid JSON"
			}
			index := &walletTestIndex{}
			index.check = func() {
				require.Equal(t, int64(7), walletBalance(t, db, user)["coins"])
				require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", user))
				require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM storage WHERE user_id = ? AND `key` = ?", user, "old"))
			}
			acks, results, err := MultiUpdate(ctx, zap.NewNop(), db, metrics, accounts, writes, deletes, index, wallets, true)
			var name string
			require.NoError(t, db.QueryRow(lifecycleSQL(db, "SELECT display_name FROM users WHERE id = ?"), user).Scan(&name))
			if failure == "success" {
				require.NoError(t, err)
				require.Equal(t, "after", name)
				require.Len(t, acks, 2)
				require.Equal(t, "z", acks[0].Key)
				require.Equal(t, "a", acks[1].Key)
				require.NotEmpty(t, acks[0].Version)
				require.NotNil(t, acks[0].CreateTime)
				require.Len(t, results, 1)
				require.Equal(t, int64(10), results[0].Previous["coins"])
				require.Equal(t, int64(7), results[0].Updated["coins"])
				require.Len(t, index.writes, 2)
				require.Len(t, index.deletes, 1)
			} else {
				require.Error(t, err)
				require.Empty(t, acks)
				require.Nil(t, results)
				require.Equal(t, "before", name)
				require.Equal(t, int64(10), walletBalance(t, db, user)["coins"])
				require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM storage WHERE user_id = ?", user))
				require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", user))
				require.Empty(t, index.writes)
				require.Empty(t, index.deletes)
			}
		})
	}
}

func TestDualBackendMultiUpdateFinalIndexAndRuntime(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	user, _ := lifecycleUser(t, db)
	index := &walletTestIndex{}
	n := &RuntimeGoNakamaModule{db: db, logger: zap.NewNop(), metrics: metrics, storageIndex: index}
	acks, results, err := n.MultiUpdate(ctx,
		[]*runtime.AccountUpdate{{UserID: user.String(), DisplayName: "atomic"}},
		[]*runtime.StorageWrite{
			{UserID: user.String(), Collection: "wallet_contract", Key: "keep", Value: `{"value":1}`},
			{UserID: user.String(), Collection: "wallet_contract", Key: "keep", Value: `{"value":2}`},
			{UserID: user.String(), Collection: "wallet_contract", Key: "gone", Value: `{"value":3}`},
		},
		[]*runtime.StorageDelete{{UserID: user.String(), Collection: "wallet_contract", Key: "gone"}},
		[]*runtime.WalletUpdate{{UserID: user.String(), Changeset: map[string]int64{"coins": 5}}}, true)
	require.NoError(t, err)
	require.Len(t, acks, 3)
	require.Len(t, results, 1)
	require.Len(t, index.writes, 1)
	require.Equal(t, "keep", index.writes[0].Key)
	require.JSONEq(t, `{"value":2}`, index.writes[0].Value)
	require.Len(t, index.deletes, 1)
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM storage WHERE user_id = ?", user))
	_, _, err = n.MultiUpdate(ctx, nil, []*runtime.StorageWrite{{UserID: user.String(), Collection: "wallet_contract", Key: "keep", Value: `{}`, Version: "stale"}}, nil, nil, false)
	require.ErrorIs(t, err, runtime.ErrStorageRejectedVersion)
}

func TestDualBackendMultiUpdateConcurrentOwners(t *testing.T) {
	db := identityTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	outcomes := make(chan error, 12)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			first, second := a, b
			if i%2 == 0 {
				first, second = b, a
			}
			_, _, err := MultiUpdate(ctx, zap.NewNop(), db, metrics, []*accountUpdate{{userID: first, displayName: wrapperspb.String("concurrent")}}, StorageOpWrites{walletStorageWrite(second, fmt.Sprint(i), `{}`, "*")}, nil, &walletTestIndex{}, []*walletUpdate{{UserID: second, Changeset: map[string]int64{"coins": 1}, Metadata: "{}"}, {UserID: first, Changeset: map[string]int64{"coins": 1}, Metadata: "{}"}}, true)
			outcomes <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	for err := range outcomes {
		require.NoError(t, err)
	}
	for _, id := range []uuid.UUID{a, b} {
		require.Equal(t, int64(12), walletBalance(t, db, id)["coins"])
		require.Equal(t, 12, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", id))
	}
}

func TestDualBackendStorageIndexAcknowledgementOrder(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprint(multi), func(t *testing.T) {
			user, _ := lifecycleUser(t, db)
			writes := StorageOpWrites{walletStorageWrite(user, "z", `{"z":1}`, ""), walletStorageWrite(user, "a", `{"a":2}`, "")}
			index := &walletTestIndex{}
			var acks []*api.StorageObjectAck
			var err error
			if multi {
				acks, _, err = MultiUpdate(ctx, logger, db, metrics, nil, writes, nil, index, nil, false)
			} else {
				result, _, writeErr := StorageWriteObjects(ctx, logger, db, metrics, index, true, writes)
				err = writeErr
				if result != nil {
					acks = result.Acks
				}
			}
			require.NoError(t, err)
			require.Len(t, index.writes, 2)
			// Index batch order is internal; each object's value/version must agree
			// with its matching caller-order acknowledgement and committed row.
			for _, object := range index.writes {
				i := 0
				if object.Key == "a" {
					i = 1
				}
				require.Equal(t, writes[i].Object.Key, object.Key)
				require.JSONEq(t, writes[i].Object.Value, object.Value)
				require.Equal(t, acks[i].Version, object.Version)
				var storedVersion string
				require.NoError(t, db.QueryRow(lifecycleSQL(db, "SELECT version FROM storage WHERE user_id = ? AND collection = ? AND `key` = ?"), user, object.Collection, object.Key).Scan(&storedVersion))
				require.Equal(t, storedVersion, object.Version)
			}
		})
	}
}
