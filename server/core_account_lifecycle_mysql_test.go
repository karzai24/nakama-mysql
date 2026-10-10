// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Fixture SQL only: production code always chooses its own native dialect.
func lifecycleSQL(db *sql.DB, query string) string {

	return query

}
func lifecycleExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.Exec(lifecycleSQL(db, query), args...)
	require.NoError(t, err)
}
func lifecycleCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(lifecycleSQL(db, query), args...).Scan(&n))
	return n
}
func lifecycleUser(t *testing.T, db *sql.DB) (uuid.UUID, string) {
	t.Helper()
	custom := GenerateString()
	id, _, _, err := AuthenticateCustom(context.Background(), zap.NewNop(), db, custom, GenerateString(), true)
	require.NoError(t, err)
	cleanupIdentityUser(t, db, id)
	return uuid.Must(uuid.FromString(id)), custom
}

type lifecycleStatus struct {
	StatusRegistry
	online string
}

func (s lifecycleStatus) FillOnlineUsers(users []*api.User) {
	for _, u := range users {
		u.Online = u.Id == s.online
	}
}
func (s lifecycleStatus) FillOnlineAccounts(accounts []*api.Account) {
	for _, a := range accounts {
		a.User.Online = a.User.Id == s.online
	}
}
func (s lifecycleStatus) IsOnline(id uuid.UUID) bool { return id.String() == s.online }

type lifecycleTracker struct {
	Tracker
	presences map[uuid.UUID][]*PresenceID
}

func (s lifecycleTracker) ListPresenceIDByStream(stream PresenceStream) []*PresenceID {
	return s.presences[stream.Subject]
}

type lifecycleSessions struct {
	SessionRegistry
	disconnected []uuid.UUID
	bans         []bool
	check        func()
}

func (s *lifecycleSessions) Disconnect(_ context.Context, id uuid.UUID, ban bool, _ ...runtime.PresenceReason) error {
	if s.check != nil {
		s.check()
	}
	s.disconnected = append(s.disconnected, id)
	s.bans = append(s.bans, ban)
	return nil
}

type lifecycleRanks struct {
	LeaderboardRankCache
	removed []LeaderboardWithExpiry
	check   func()
}

func (s *lifecycleRanks) Delete(id string, expiry int64, _ uuid.UUID) bool {
	if s.check != nil {
		s.check()
	}
	s.removed = append(s.removed, LeaderboardWithExpiry{LeaderboardId: id, Expiry: expiry})
	return true
}

func TestDualBackendAccountLookups(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	c, _ := lifecycleUser(t, db)
	deviceA, deviceB, fb := GenerateString(), GenerateString(), GenerateString()
	require.NoError(t, LinkDevice(ctx, logger, db, a, deviceA))
	require.NoError(t, LinkDevice(ctx, logger, db, a, deviceB))
	lifecycleExec(t, db, "UPDATE users SET facebook_id = ?, display_name = ?, metadata = ?, wallet = ? WHERE id = ?", fb, "فارسی 🂡", `{"nested":{"score":7}}`, `{"coins":42}`, b)
	registry := lifecycleStatus{online: a.String()}
	accountB, err := GetAccount(ctx, logger, db, registry, b)
	require.NoError(t, err)
	for _, tc := range []struct {
		ids, devices []string
		want         []uuid.UUID
	}{
		{nil, nil, nil}, {[]string{a.String(), a.String(), uuid.Must(uuid.NewV4()).String()}, nil, []uuid.UUID{a}},
		{nil, []string{deviceA, deviceB, "' OR 1=1 --"}, []uuid.UUID{a}},
		{[]string{b.String()}, []string{deviceA}, []uuid.UUID{a, b}},
		{[]string{c.String()}, nil, []uuid.UUID{c}},
		{[]string{strings.ToUpper(b.String())}, nil, []uuid.UUID{b}},
	} {
		accounts, err := GetAccounts(ctx, logger, db, registry, tc.ids, tc.devices)
		require.NoError(t, err)
		require.Len(t, accounts, len(tc.want))
		got := []uuid.UUID{}
		for _, account := range accounts {
			id := uuid.Must(uuid.FromString(account.User.Id))
			got = append(got, id)
			single, err := GetAccount(ctx, logger, db, registry, id)
			require.NoError(t, err)
			require.Equal(t, single.User, account.User)
			require.JSONEq(t, single.Wallet, account.Wallet)
			require.Equal(t, single.CustomId, account.CustomId)
			require.Equal(t, single.Email, account.Email)
			require.Nil(t, account.VerifyTime)
			require.Nil(t, account.DisableTime)
			require.ElementsMatch(t, single.Devices, account.Devices)
		}
		require.ElementsMatch(t, tc.want, got)
	}
	for _, tc := range []struct {
		ids, names, fb []string
		want           []uuid.UUID
	}{
		{[]string{a.String(), a.String()}, []string{accountB.User.Username}, []string{fb}, []uuid.UUID{a, b}},
		{nil, []string{accountB.User.Username}, nil, []uuid.UUID{b}},
		{nil, nil, []string{fb}, []uuid.UUID{b}},
		{[]string{strings.ToUpper(a.String())}, nil, nil, []uuid.UUID{a}},
		{nil, []string{strings.ToUpper(accountB.User.Username), accountB.User.Username + " ", "' OR 1=1 --"}, nil, nil},
	} {
		users, err := GetUsers(ctx, logger, db, registry, tc.ids, tc.names, tc.fb)
		require.NoError(t, err)
		got := []uuid.UUID{}
		for _, u := range users.Users {
			got = append(got, uuid.Must(uuid.FromString(u.Id)))
			require.Equal(t, u.Id == a.String(), u.Online)
		}
		require.ElementsMatch(t, tc.want, got)
	}
	ids, err := fetchUserID(ctx, db, []string{accountB.User.Username, accountB.User.Username, "' OR 1=1 --"})
	require.NoError(t, err)
	require.Equal(t, []string{b.String()}, ids)
	ids, err = fetchUserID(ctx, db, nil)
	require.NoError(t, err)
	require.Empty(t, ids)
	random, err := GetRandomUsers(ctx, logger, db, registry, 3)
	require.NoError(t, err)
	require.Len(t, random, 3)
	seen := map[string]bool{}
	for _, u := range random {
		require.NotEqual(t, uuid.Nil.String(), u.Id)
		require.False(t, seen[u.Id])
		seen[u.Id] = true
	}
	random, err = GetRandomUsers(ctx, logger, db, registry, 0)
	require.NoError(t, err)
	require.Empty(t, random)
	exists, err := UserExistsAndDoesNotBlock(ctx, db, a, b)
	require.NoError(t, err)
	require.True(t, exists)
	lifecycleExec(t, db, "INSERT INTO user_edge (source_id,destination_id,state,`position`) VALUES (?,?,3,1)", a, b)
	exists, err = UserExistsAndDoesNotBlock(ctx, db, a, b)
	require.NoError(t, err)
	require.False(t, exists)
	exists, err = UserExistsAndDoesNotBlock(ctx, db, b, a)
	require.NoError(t, err)
	require.True(t, exists)
	exists, err = UserExistsAndDoesNotBlock(ctx, db, uuid.Must(uuid.NewV4()), a)
	require.NoError(t, err)
	require.False(t, exists)
}

func TestDualBackendAccountModeration(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	a, custom := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	config := NewConfig(zap.NewNop())
	cache := NewLocalSessionCache(config.GetSession().TokenExpirySec, config.GetSession().RefreshTokenExpirySec)
	t.Cleanup(cache.Stop)
	tokenID := uuid.Must(uuid.NewV4()).String()
	token, refreshExp := generateTokenWithExpiry(config.GetSession().RefreshEncryptionKey, tokenID, time.Now().Unix()-10, a.String(), "", nil, time.Now().Unix()+config.GetSession().RefreshTokenExpirySec-10)
	_, _, _, _, _, err := SessionRefresh(ctx, logger, db, config, cache, token)
	require.NoError(t, err)
	sessionID := uuid.Must(uuid.NewV4())
	tracker := lifecycleTracker{presences: map[uuid.UUID][]*PresenceID{a: {{SessionID: sessionID}}}}
	sessions := &lifecycleSessions{}
	ids := []uuid.UUID{a, b, uuid.Must(uuid.NewV4())}
	require.NoError(t, BanUsers(ctx, logger, db, config, cache, sessions, tracker, ids))
	require.Equal(t, []uuid.UUID{sessionID}, sessions.disconnected)
	require.Equal(t, []bool{true}, sessions.bans)
	require.False(t, cache.IsValidRefresh(a, refreshExp, tokenID))
	for _, id := range []uuid.UUID{a, b} {
		acc, err := GetAccount(ctx, logger, db, nil, id)
		require.NoError(t, err)
		require.NotNil(t, acc.DisableTime)
	}
	_, _, _, err = AuthenticateCustom(ctx, logger, db, custom, "", false)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	// A fresh cache (server restart) still reads the persisted ban on refresh.
	fresh := NewLocalSessionCache(config.GetSession().TokenExpirySec, config.GetSession().RefreshTokenExpirySec)
	t.Cleanup(fresh.Stop)
	_, _, _, _, _, err = SessionRefresh(ctx, logger, db, config, fresh, token)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.NoError(t, UnbanUsers(ctx, logger, db, cache, ids))
	_, _, _, err = AuthenticateCustom(ctx, logger, db, custom, "", false)
	require.NoError(t, err)
	acc, err := GetAccount(ctx, logger, db, nil, a)
	require.NoError(t, err)
	require.Nil(t, acc.DisableTime)
	require.False(t, cache.IsValidRefresh(a, refreshExp, tokenID), "unban must not resurrect revoked tokens")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, BanUsers(canceled, zap.NewNop(), db, config, fresh, sessions, tracker, []uuid.UUID{a}))
	require.True(t, fresh.IsValidRefresh(a, refreshExp, tokenID))
	require.NoError(t, BanUsers(ctx, logger, db, config, cache, sessions, tracker, nil))
	require.NoError(t, UnbanUsers(ctx, logger, db, cache, nil))
}

// Seed all dependent table types directly; their public feature APIs are still
// pending. This verifies account-deletion cleanup without claiming those APIs.
type lifecycleData struct {
	user, peer                           uuid.UUID
	board, purchase, subscription, group string
}

func seedLifecycleData(t *testing.T, db *sql.DB) lifecycleData {
	t.Helper()
	u, _ := lifecycleUser(t, db)
	peer, _ := lifecycleUser(t, db)
	f := lifecycleData{u, peer, GenerateString(), GenerateString(), GenerateString(), uuid.Must(uuid.NewV4()).String()}
	require.NoError(t, LinkDevice(context.Background(), logger, db, u, GenerateString()))
	lifecycleExec(t, db, "INSERT INTO storage (collection,`key`,user_id,value,version) VALUES ('lifecycle','one',?,'{}','test')", u)
	lifecycleExec(t, db, "INSERT INTO notification (id,user_id,subject,code,sender_id) VALUES (?,?,'test',1,?)", uuid.Must(uuid.NewV4()), u, peer)
	lifecycleExec(t, db, "INSERT INTO users_notes (id,user_id,note) VALUES (?,?,'test')", uuid.Must(uuid.NewV4()), u)
	lifecycleExec(t, db, "INSERT INTO wallet_ledger (id,user_id,changeset,metadata) VALUES (?,?,'{}','{}')", uuid.Must(uuid.NewV4()), u)
	lifecycleExec(t, db, "INSERT INTO message (id,sender_id,username,stream_mode,stream_subject,stream_descriptor,stream_label) VALUES (?,?,'test',2,?,?,'test')", uuid.Must(uuid.NewV4()), u, uuid.Nil, uuid.Nil)
	lifecycleExec(t, db, "INSERT INTO purchase (transaction_id,user_id,product_id) VALUES (?,?,'test')", f.purchase, u)
	lifecycleExec(t, db, "INSERT INTO subscription (original_transaction_id,user_id,product_id,expire_time) VALUES (?,?,'test',?)", f.subscription, u, time.Now().UTC().Add(time.Hour))
	lifecycleExec(t, db, "INSERT INTO user_edge (source_id,destination_id,state,`position`) VALUES (?,?,0,1),(?,?,0,1)", u, peer, peer, u)
	lifecycleExec(t, db, "UPDATE users SET edge_count = 1 WHERE id = ? OR id = ?", u, peer)
	lifecycleExec(t, db, "INSERT INTO leaderboard (id) VALUES (?)", f.board)
	for _, expiry := range []time.Time{time.Unix(0, 0).UTC(), time.Now().UTC().Add(time.Hour).Truncate(time.Second), time.Unix(1, 0).UTC()} {
		lifecycleExec(t, db, "INSERT INTO leaderboard_record (leaderboard_id,owner_id,expiry_time) VALUES (?,?,?)", f.board, u, expiry)
	}
	lifecycleExec(t, db, "INSERT INTO `groups` (id,creator_id,name,edge_count) VALUES (?,?,?,1)", f.group, u, GenerateString())
	lifecycleExec(t, db, "INSERT INTO group_edge (source_id,destination_id,state,`position`) VALUES (?,?,0,1),(?,?,0,1)", f.group, u, u, f.group)
	t.Cleanup(func() {
		lifecycleExec(t, db, "DELETE FROM group_edge WHERE source_id = ? OR destination_id = ?", f.group, f.group)
		lifecycleExec(t, db, "DELETE FROM `groups` WHERE id = ?", f.group)
		lifecycleExec(t, db, "DELETE FROM leaderboard WHERE id = ?", f.board)
		lifecycleExec(t, db, "DELETE FROM purchase WHERE transaction_id = ?", f.purchase)
		lifecycleExec(t, db, "DELETE FROM subscription WHERE original_transaction_id = ?", f.subscription)
		lifecycleExec(t, db, "DELETE FROM user_tombstone WHERE user_id = ?", u)
	})
	return f
}
func assertLifecycleDeleted(t *testing.T, db *sql.DB, f lifecycleData) {
	t.Helper()
	for _, tc := range []struct{ table, column string }{
		{"users", "id"}, {"user_device", "user_id"}, {"storage", "user_id"}, {"notification", "user_id"},
		{"users_notes", "user_id"}, {"wallet_ledger", "user_id"}, {"message", "sender_id"}, {"leaderboard_record", "owner_id"},
		{"user_edge", "source_id"}, {"user_edge", "destination_id"}, {"group_edge", "source_id"}, {"group_edge", "destination_id"},
	} {
		require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM "+tc.table+" WHERE "+tc.column+" = ?", f.user), tc.table+" "+tc.column)
	}
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM `groups` WHERE id = ?", f.group))
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM purchase WHERE transaction_id = ? AND user_id = ?", f.purchase, uuid.Nil))
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM subscription WHERE original_transaction_id = ? AND user_id = ?", f.subscription, uuid.Nil))
	require.Zero(t, lifecycleCount(t, db, "SELECT edge_count FROM users WHERE id = ?", f.peer))
}
func TestDualBackendAccountDeletion(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	f := seedLifecycleData(t, db)
	cache := NewLocalSessionCache(60, 600)
	t.Cleanup(cache.Stop)
	ranks := &lifecycleRanks{}
	sid := uuid.Must(uuid.NewV4())
	sessions := &lifecycleSessions{check: func() { assertLifecycleDeleted(t, db, f) }}
	tracker := lifecycleTracker{presences: map[uuid.UUID][]*PresenceID{f.user: {{SessionID: sid}}}}
	ranks.check = func() { assertLifecycleDeleted(t, db, f) }
	require.NoError(t, DeleteAccount(ctx, logger, db, nil, nil, ranks, sessions, cache, tracker, f.user, true, nil))
	assertLifecycleDeleted(t, db, f)
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM user_tombstone WHERE user_id = ?", f.user))
	require.Len(t, ranks.removed, 2)
	require.Equal(t, []uuid.UUID{sid}, sessions.disconnected)
	require.Equal(t, []bool{false}, sessions.bans)
	require.False(t, cache.IsValidSession(f.user, time.Now().Unix()+50, "old"))
	require.False(t, cache.IsValidRefresh(f.user, time.Now().Unix()+590, "old"))
	require.NoError(t, DeleteAccount(ctx, logger, db, nil, nil, ranks, sessions, cache, tracker, f.user, true, nil))
	require.Len(t, ranks.removed, 2)
	require.Len(t, sessions.disconnected, 1)
	require.Error(t, DeleteAccount(ctx, logger, db, nil, nil, ranks, sessions, cache, tracker, uuid.Nil, true, nil))
	missing := uuid.Must(uuid.NewV4())
	require.NoError(t, DeleteAccount(ctx, logger, db, nil, nil, ranks, sessions, cache, tracker, missing, true, nil))
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM user_tombstone WHERE user_id = ?", missing))
}

func TestDualBackendAccountDeletionRollbackAndIndex(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	f := seedLifecycleData(t, db)
	index, err := NewLocalStorageIndex(logger, db, &StorageConfig{}, metrics)
	require.NoError(t, err)
	require.NoError(t, index.CreateIndex(ctx, "lifecycle", "lifecycle", "", []string{"value"}, nil, 100, true))
	for _, owner := range []uuid.UUID{f.user, f.peer} {
		_, _, err = StorageWriteObjects(ctx, logger, db, metrics, index, true, StorageOpWrites{{OwnerID: owner.String(), Object: &api.WriteStorageObject{Collection: "lifecycle", Key: "indexed", Value: `{"value":7}`}}})
		require.NoError(t, err)
	}
	indexed, _, err := index.List(ctx, uuid.Nil, "lifecycle", "*", 100, nil, "")
	require.NoError(t, err)
	require.Len(t, indexed.Objects, 2)
	cache := NewLocalSessionCache(60, 600)
	t.Cleanup(cache.Stop)
	ranks, sessions := &lifecycleRanks{}, &lifecycleSessions{}
	sid := uuid.Must(uuid.NewV4())
	tracker := lifecycleTracker{presences: map[uuid.UUID][]*PresenceID{f.user: {{SessionID: sid}}}}
	// Fail after all deletes, including the purchase-retention trigger, have run.
	// Unique trigger matches only this disposable account and is removed by cleanup.
	dropFailure := failureTrigger(t, db, "user_tombstone", "INSERT", "NEW.user_id = '"+f.user.String()+"'")
	require.Error(t, DeleteAccount(ctx, zap.NewNop(), db, nil, nil, ranks, sessions, cache, tracker, f.user, true, index))
	require.Empty(t, ranks.removed)
	require.Empty(t, sessions.disconnected)
	require.True(t, cache.IsValidSession(f.user, time.Now().Unix()+50, "old"))
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM users WHERE id = ?", f.user))
	require.Equal(t, 3, lifecycleCount(t, db, "SELECT COUNT(*) FROM leaderboard_record WHERE owner_id = ?", f.user))
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM purchase WHERE transaction_id = ? AND user_id = ?", f.purchase, f.user))
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT edge_count FROM users WHERE id = ?", f.peer))
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM `groups` WHERE id = ?", f.group))
	indexed, _, err = index.List(ctx, uuid.Nil, "lifecycle", "*", 100, nil, "")
	require.NoError(t, err)
	require.Len(t, indexed.Objects, 2)
	dropFailure()
	require.NoError(t, DeleteAccount(ctx, logger, db, nil, nil, ranks, sessions, cache, tracker, f.user, true, index))
	assertLifecycleDeleted(t, db, f)
	indexed, _, err = index.List(ctx, uuid.Nil, "lifecycle", "*", 100, nil, "")
	require.NoError(t, err)
	require.Len(t, indexed.Objects, 1)
	require.Equal(t, f.peer.String(), indexed.Objects[0].UserId)
}

func TestDualBackendAccountGroupCleanup(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	for _, state := range []int{0, 1, 2, 3, 4} {
		t.Run(fmt.Sprint(state), func(t *testing.T) {
			user, _ := lifecycleUser(t, db)
			peer, _ := lifecycleUser(t, db)
			group := uuid.Must(uuid.NewV4())
			count := 1
			if state < 3 {
				count++
			}
			lifecycleExec(t, db, "INSERT INTO `groups` (id,creator_id,name,edge_count) VALUES (?,?,?,?)", group, peer, GenerateString(), count)
			lifecycleExec(t, db, "INSERT INTO group_edge (source_id,destination_id,state,`position`) VALUES (?,?,?,1),(?,?,?,1),(?,?,0,2),(?,?,0,2)", group, user, state, user, group, state, group, peer, peer, group)
			t.Cleanup(func() {
				lifecycleExec(t, db, "DELETE FROM group_edge WHERE source_id = ? OR destination_id = ?", group, group)
				lifecycleExec(t, db, "DELETE FROM `groups` WHERE id = ?", group)
			})
			cache := NewLocalSessionCache(60, 600)
			t.Cleanup(cache.Stop)
			require.NoError(t, DeleteAccount(ctx, logger, db, nil, nil, &lifecycleRanks{}, &lifecycleSessions{}, cache, lifecycleTracker{}, user, false, nil))
			require.Equal(t, 1, lifecycleCount(t, db, "SELECT edge_count FROM `groups` WHERE id = ?", group))
			require.Equal(t, 2, lifecycleCount(t, db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? OR destination_id = ?", group, group))
			require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? OR destination_id = ?", user, user))
			require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM user_tombstone WHERE user_id = ?", user))
		})
	}
}

func TestDualBackendConcurrentAccountDeletion(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	group := uuid.Must(uuid.NewV4())
	lifecycleExec(t, db, "INSERT INTO `groups` (id,creator_id,name,edge_count) VALUES (?,?,?,2)", group, a, GenerateString())
	lifecycleExec(t, db, "INSERT INTO group_edge (source_id,destination_id,state,`position`) VALUES (?,?,0,1),(?,?,0,1),(?,?,0,2),(?,?,0,2)", group, a, a, group, group, b, b, group)
	lifecycleExec(t, db, "INSERT INTO user_edge (source_id,destination_id,state,`position`) VALUES (?,?,0,1),(?,?,0,1)", a, b, b, a)
	lifecycleExec(t, db, "UPDATE users SET edge_count = 1 WHERE id = ? OR id = ?", a, b)
	t.Cleanup(func() {
		lifecycleExec(t, db, "DELETE FROM group_edge WHERE source_id = ? OR destination_id = ?", group, group)
		lifecycleExec(t, db, "DELETE FROM `groups` WHERE id = ?", group)
	})
	cache := NewLocalSessionCache(60, 600)
	t.Cleanup(cache.Stop)
	gate := make(chan struct{})
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for _, id := range []uuid.UUID{a, a, b, b} {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			<-gate
			errs <- DeleteAccount(ctx, zap.NewNop(), db, nil, nil, &lifecycleRanks{}, &lifecycleSessions{}, cache, lifecycleTracker{}, id, false, nil)
		}(id)
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM `groups` WHERE id = ?", group))
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? OR destination_id = ?", group, group))
}
