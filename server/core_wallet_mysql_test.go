// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func walletBalance(t *testing.T, db *sql.DB, user uuid.UUID) map[string]int64 {
	t.Helper()
	var raw string
	require.NoError(t, db.QueryRow(lifecycleSQL(db, "SELECT wallet FROM users WHERE id = ?"), user).Scan(&raw))
	var result map[string]int64
	require.NoError(t, json.Unmarshal([]byte(raw), &result))
	return result
}

func TestDualBackendWalletLifecycle(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	user, _ := lifecycleUser(t, db)
	n := &RuntimeGoNakamaModule{db: db, logger: zap.NewNop()}
	large := int64(9007199254740993)
	updated, previous, err := n.WalletUpdate(ctx, user.String(), map[string]int64{"coins": large}, map[string]interface{}{"source": "فارسی", "nested": map[string]interface{}{"old": true}, "keep": true}, true)
	require.NoError(t, err)
	require.Empty(t, previous)
	require.Equal(t, large, updated["coins"])
	updated, previous, err = n.WalletUpdate(ctx, user.String(), map[string]int64{"coins": -3}, nil, false)
	require.NoError(t, err)
	require.Equal(t, large, previous["coins"])
	require.Equal(t, large-3, updated["coins"])
	require.Equal(t, updated, walletBalance(t, db, user))
	entries, next, prev, err := ListWalletLedger(ctx, logger, db, user, nil, "", time.Time{}, time.Time{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Empty(t, next)
	require.Empty(t, prev)
	require.Equal(t, large, entries[0].Changeset["coins"])
	entry, err := n.WalletLedgerUpdate(ctx, entries[0].ID, map[string]interface{}{"source": nil, "nested": map[string]interface{}{"new": true}, "exact": large})
	require.NoError(t, err)
	require.Equal(t, user.String(), entry.GetUserID())
	var raw string
	require.NoError(t, db.QueryRow(lifecycleSQL(db, "SELECT metadata FROM wallet_ledger WHERE id = ?"), entries[0].ID).Scan(&raw))
	require.JSONEq(t, `{"source":null,"nested":{"new":true},"keep":true,"exact":9007199254740993}`, raw)
	require.Equal(t, entries[0].ID, entry.GetID())
	require.Contains(t, entry.GetMetadata(), "source")
	require.Equal(t, user.String(), entries[0].UserID)
	_, err = n.WalletLedgerUpdate(ctx, uuid.Must(uuid.NewV4()).String(), nil)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, _, err = n.WalletUpdate(ctx, uuid.Must(uuid.NewV4()).String(), map[string]int64{"coins": 1}, nil, true)
	require.EqualError(t, err, "user not found")
	server := &ConsoleServer{db: db, logger: zap.NewNop()}
	page, err := server.GetWalletLedger(ctx, &console.GetWalletLedgerRequest{AccountId: user.String(), Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	other, _ := lifecycleUser(t, db)
	_, err = server.DeleteWalletLedger(ctx, &console.DeleteWalletLedgerRequest{Id: other.String(), WalletId: entries[0].ID})
	require.NoError(t, err)
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", user))
	_, err = server.DeleteWalletLedger(ctx, &console.DeleteWalletLedgerRequest{Id: user.String(), WalletId: entries[0].ID})
	require.NoError(t, err)
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", user))
	require.Equal(t, large-3, walletBalance(t, db, user)["coins"], "ledger removal must not change balance")
}

func TestDualBackendWalletAtomicBatch(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	update := func(id uuid.UUID, amount int64, metadata string) *walletUpdate {
		return &walletUpdate{UserID: id, Changeset: map[string]int64{"coins": amount}, Metadata: metadata}
	}
	_, err := UpdateWallets(ctx, logger, db, []*walletUpdate{update(a, 10, "{}"), update(b, 20, "{}")}, true)
	require.NoError(t, err)
	for _, batch := range [][]*walletUpdate{
		{update(a, 5, "{}"), update(b, -21, "{}")},
		{update(a, 5, "{}"), update(b, 5, "invalid JSON")},
	} {
		results, err := UpdateWallets(ctx, zap.NewNop(), db, batch, true)
		require.Error(t, err)
		require.Nil(t, results)
		require.Equal(t, int64(10), walletBalance(t, db, a)["coins"])
		require.Equal(t, int64(20), walletBalance(t, db, b)["coins"])
		require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", a))
		require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", b))
	}
	results, err := UpdateWallets(ctx, logger, db, []*walletUpdate{update(uuid.Must(uuid.NewV4()), 4, "{}"), update(a, -10, "ignored invalid metadata")}, false)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Zero(t, walletBalance(t, db, a)["coins"])
}

func TestDualBackendWalletBoundsAndSnapshots(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	user, _ := lifecycleUser(t, db)
	result, err := UpdateWallets(ctx, logger, db, []*walletUpdate{
		{UserID: user, Changeset: map[string]int64{"coins": 5}, Metadata: "{}"},
		{UserID: user, Changeset: map[string]int64{"coins": -2}, Metadata: "{}"},
	}, true)
	require.NoError(t, err)
	require.Equal(t, int64(5), result[0].Updated["coins"])
	require.Equal(t, int64(5), result[1].Previous["coins"])
	require.Equal(t, int64(3), result[1].Updated["coins"])
	result[1].Previous["coins"] = 999
	require.Equal(t, int64(5), result[0].Updated["coins"])
	for _, invalid := range []string{`null`, `[]`, `{"coins":1.5}`, `{"coins":-1}`, `{"coins":9223372036854775808}`} {
		lifecycleExec(t, db, "UPDATE users SET wallet = ? WHERE id = ?", invalid, user)
		_, err = UpdateWallets(ctx, zap.NewNop(), db, []*walletUpdate{{UserID: user, Changeset: map[string]int64{"coins": 1}}}, false)
		require.Error(t, err, invalid)
	}
	lifecycleExec(t, db, "UPDATE users SET wallet = ? WHERE id = ?", `{"coins":9223372036854775807}`, user)
	result, err = UpdateWallets(ctx, zap.NewNop(), db, []*walletUpdate{{UserID: user, Changeset: map[string]int64{"coins": 1}}}, false)
	require.ErrorIs(t, err, errWalletOverflow)
	require.Nil(t, result)
	require.Equal(t, int64(math.MaxInt64), walletBalance(t, db, user)["coins"])
	_, err = UpdateWallets(ctx, zap.NewNop(), db, []*walletUpdate{{UserID: user, Changeset: map[string]int64{"coins": math.MinInt64}}}, false)
	var negative *runtime.WalletNegativeError
	require.ErrorAs(t, err, &negative)
	require.Equal(t, int64(math.MaxInt64), negative.Current)
	require.Equal(t, int64(math.MinInt64), negative.Amount)
}

func TestDualBackendWalletConcurrency(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	user, _ := lifecycleUser(t, db)
	_, err := UpdateWallets(ctx, logger, db, []*walletUpdate{{UserID: user, Changeset: map[string]int64{"coins": 5}}}, false)
	require.NoError(t, err)
	for _, amount := range []int64{-1, 1} {
		start, outcomes := make(chan struct{}), make(chan error, 16)
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := UpdateWallets(ctx, zap.NewNop(), db, []*walletUpdate{{UserID: user, Changeset: map[string]int64{"coins": amount}, Metadata: "{}"}}, true)
				outcomes <- err
			}()
		}
		close(start)
		wg.Wait()
		close(outcomes)
		successes := 0
		for err := range outcomes {
			if err == nil {
				successes++
			} else {
				var insufficient *runtime.WalletNegativeError
				require.True(t, errors.As(err, &insufficient), "unexpected concurrency failure: %v", err)
			}
		}
		if amount < 0 {
			require.Equal(t, 5, successes)
			require.Zero(t, walletBalance(t, db, user)["coins"])
		} else {
			require.Equal(t, 16, successes)
			require.Equal(t, int64(16), walletBalance(t, db, user)["coins"])
		}
	}
	require.Equal(t, 21, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", user))
}

func TestDualBackendWalletLedgerPagination(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	user, _ := lifecycleUser(t, db)
	other, _ := lifecycleUser(t, db)
	ids := make([]string, 5)
	epoch := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i := range ids {
		ids[i] = uuid.Must(uuid.NewV4()).String()
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	for _, id := range ids {
		stamp := epoch // The order must use the same tie-break as the cursor.
		lifecycleExec(t, db, "INSERT INTO wallet_ledger (id,user_id,changeset,metadata,create_time,update_time) VALUES (?,?,?,?,?,?)", id, user, `{"coins":1}`, `{}`, stamp, stamp)
	}
	limit := 2
	after, before := epoch.Add(-time.Second), epoch.Add(time.Second)
	list := func(cursor string) ([]*walletLedger, string, string) {
		page, next, prev, err := ListWalletLedger(ctx, logger, db, user, &limit, cursor, after, before)
		require.NoError(t, err)
		return page, next, prev
	}
	first, next, prev := list("")
	require.Len(t, first, 2)
	require.Equal(t, ids[0], first[0].ID)
	require.Empty(t, prev)
	require.NotEmpty(t, next)
	second, next2, prev2 := list(next)
	require.Len(t, second, 2)
	require.Equal(t, ids[2], second[0].ID)
	require.NotEmpty(t, next2)
	require.NotEmpty(t, prev2)
	back, _, backPrev := list(prev2)
	require.Equal(t, first, back)
	require.Empty(t, backPrev)
	third, last, prev3 := list(next2)
	require.Len(t, third, 1)
	require.Equal(t, ids[4], third[0].ID)
	require.Empty(t, last)
	require.NotEmpty(t, prev3)
	back2, _, _ := list(prev3)
	require.Equal(t, second, back2)
	all, _, _, err := ListWalletLedger(ctx, logger, db, user, nil, "", after, before)
	require.NoError(t, err)
	require.Len(t, all, 5)
	for _, cursor := range []string{"not a cursor", next} {
		_, _, _, err = ListWalletLedger(ctx, zap.NewNop(), db, other, &limit, cursor, after, before)
		require.ErrorIs(t, err, runtime.ErrWalletLedgerInvalidCursor)
	}
	_, _, _, err = ListWalletLedger(ctx, zap.NewNop(), db, user, &limit, next, epoch.Add(-2*time.Second), before)
	require.ErrorIs(t, err, runtime.ErrWalletLedgerInvalidCursor)
}

func TestDualBackendWalletAccountDeletionRace(t *testing.T) {
	db := identityTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cache := NewLocalSessionCache(60, 600)
	t.Cleanup(cache.Stop)
	for i := 0; i < 8; i++ {
		user, _ := lifecycleUser(t, db)
		_, err := UpdateWallets(ctx, logger, db, []*walletUpdate{{UserID: user, Changeset: map[string]int64{"coins": 1}, Metadata: "{}"}}, true)
		require.NoError(t, err)
		entries, _, _, err := ListWalletLedger(ctx, logger, db, user, nil, "", time.Time{}, time.Time{})
		require.NoError(t, err)
		require.Len(t, entries, 1)
		start := make(chan struct{})
		outcomes := make(chan error, 3)
		go func() {
			<-start
			_, err := UpdateWallets(ctx, zap.NewNop(), db, []*walletUpdate{{UserID: user, Changeset: map[string]int64{"coins": 1}, Metadata: "{}"}}, true)
			outcomes <- err
		}()
		go func() {
			<-start
			_, err := UpdateWalletLedger(ctx, zap.NewNop(), db, uuid.Must(uuid.FromString(entries[0].ID)), `{"race":true}`)
			outcomes <- err
		}()
		go func() {
			<-start
			outcomes <- DeleteAccount(ctx, zap.NewNop(), db, nil, nil, &lifecycleRanks{}, &lifecycleSessions{}, cache, lifecycleTracker{}, user, false, nil)
		}()
		close(start)
		for j := 0; j < 3; j++ {
			err := <-outcomes
			if err != nil {
				require.ErrorIs(t, err, sql.ErrNoRows)
			}
		}
		require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM users WHERE id = ?", user))
		require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?", user))
	}
}

func TestDualBackendWalletLedgerInvalidMergeRollback(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	owner, _ := lifecycleUser(t, db)
	_, err := UpdateWallets(ctx, logger, db, []*walletUpdate{{UserID: owner, Changeset: map[string]int64{"coins": 1}, Metadata: `{"keep":true}`}}, true)
	require.NoError(t, err)
	entries, _, _, err := ListWalletLedger(ctx, logger, db, owner, nil, "", time.Time{}, time.Time{})
	require.NoError(t, err)
	id := uuid.Must(uuid.FromString(entries[0].ID))
	for _, invalid := range []string{`[]`, `null`, `42`, `invalid JSON`} {
		item, err := UpdateWalletLedger(ctx, zap.NewNop(), db, id, invalid)
		require.Error(t, err, invalid)
		require.Nil(t, item)
		var stored string
		require.NoError(t, db.QueryRow(lifecycleSQL(db, "SELECT metadata FROM wallet_ledger WHERE id = ?"), id).Scan(&stored))
		require.JSONEq(t, `{"keep":true}`, stored)
	}
}
