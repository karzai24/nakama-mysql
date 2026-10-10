// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type friendTracker struct {
	Tracker
	untracked []PresenceStream
	check     func()
}

func (t *friendTracker) ListPresenceIDByStreams(_ map[PresenceStream][]*PresenceID) {}
func (t *friendTracker) UntrackByStream(s PresenceStream) {
	if t.check != nil {
		t.check()
	}
	t.untracked = append(t.untracked, s)
}

type friendStatus struct{ lifecycleStatus }

func (s friendStatus) FillOnlineFriends(friends []*api.Friend) {
	for _, f := range friends {
		f.User.Online = f.User.Id == s.online
	}
}
func friendAdd(t *testing.T, db *sql.DB, a, b uuid.UUID, metadata string) {
	t.Helper()
	require.NoError(t, AddFriends(context.Background(), zap.NewNop(), db, &friendTracker{}, nil, a, "fixture", []string{b.String()}, metadata))
}
func friendAccept(t *testing.T, db *sql.DB, a, b uuid.UUID) {
	t.Helper()
	friendAdd(t, db, a, b, "")
	friendAdd(t, db, b, a, "")
}
func friendState(t *testing.T, db *sql.DB, a, b uuid.UUID, want int) {
	t.Helper()
	list, err := GetFriends(context.Background(), logger, db, nil, a, []uuid.UUID{b})
	require.NoError(t, err)
	if want < 0 {
		require.Empty(t, list)
		return
	}
	require.Len(t, list, 1)
	require.EqualValues(t, want, list[0].State.Value)
}
func friendCounts(t *testing.T, db *sql.DB, ids ...uuid.UUID) {
	t.Helper()
	for _, id := range ids {
		require.Equal(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM user_edge WHERE source_id = ?", id), lifecycleCount(t, db, "SELECT edge_count FROM users WHERE id = ?", id))
	}
}
func friendNotices(t *testing.T, db *sql.DB, owner uuid.UUID, code int32) int {
	t.Helper()
	return lifecycleCount(t, db, "SELECT COUNT(*) FROM notification WHERE user_id = ? AND code = ?", owner, code)
}

func TestDualBackendFriendLifecycle(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	tr := &friendTracker{}
	require.NoError(t, AddFriends(ctx, logger, db, tr, nil, a, "fixture", []string{b.String(), b.String(), uuid.Must(uuid.NewV4()).String()}, `{"from":"a"}`))
	friendState(t, db, a, b, 1)
	friendState(t, db, b, a, 2)
	friendCounts(t, db, a, b)
	require.Equal(t, 1, friendNotices(t, db, b, NotificationCodeFriendRequest))
	friendAdd(t, db, a, b, "")
	require.Equal(t, 1, friendNotices(t, db, b, NotificationCodeFriendRequest))
	require.NoError(t, UpdateFriendMetadata(ctx, logger, db, b, a, map[string]any{"keep": 7, "nested": map[string]any{"old": 1}, "nullable": "old"}))
	friendAdd(t, db, b, a, `{"nullable":null,"nested":{"new":2},"large":9007199254740993}`)
	friendState(t, db, a, b, 0)
	friendState(t, db, b, a, 0)
	friendCounts(t, db, a, b)
	got, err := GetFriends(ctx, logger, db, friendStatus{lifecycleStatus{online: a.String()}}, b, []uuid.UUID{a, a})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.True(t, got[0].User.Online)
	require.JSONEq(t, `{"keep":7,"nullable":null,"nested":{"new":2},"large":9007199254740993}`, got[0].Metadata)
	require.Contains(t, got[0].Metadata, "9007199254740993")
	require.Equal(t, 1, friendNotices(t, db, a, NotificationCodeFriendAccept))
	require.NoError(t, UpdateFriendMetadata(ctx, logger, db, b, a, nil))
	got, err = GetFriends(ctx, logger, db, nil, b, []uuid.UUID{a})
	require.NoError(t, err)
	require.JSONEq(t, "{}", got[0].Metadata)
	require.NoError(t, BlockFriends(ctx, logger, db, tr, a, []string{b.String(), b.String()}))
	friendState(t, db, a, b, 3)
	friendState(t, db, b, a, -1)
	friendCounts(t, db, a, b)
	require.Len(t, tr.untracked, 1)
	require.Equal(t, uint8(StreamModeDM), tr.untracked[0].Mode)
	friendAdd(t, db, b, a, "")
	friendAdd(t, db, a, b, "")
	friendState(t, db, a, b, 3)
	friendState(t, db, b, a, -1)
	require.NoError(t, BlockFriends(ctx, logger, db, tr, b, []string{a.String()}))
	friendState(t, db, a, b, 3)
	friendState(t, db, b, a, 3)
	require.NoError(t, DeleteFriends(ctx, logger, db, tr, nil, a, "fixture", []string{b.String()}))
	friendState(t, db, a, b, -1)
	friendState(t, db, b, a, 3)
	friendCounts(t, db, a, b)
	require.NoError(t, DeleteFriends(ctx, logger, db, tr, nil, b, "fixture", []string{a.String()}))
	friendState(t, db, b, a, -1)
	friendCounts(t, db, a, b)
	friendAccept(t, db, a, b)
	require.NoError(t, DeleteFriends(ctx, logger, db, tr, nil, a, "fixture", []string{b.String()}))
	friendState(t, db, a, b, -1)
	friendState(t, db, b, a, -1)
	friendCounts(t, db, a, b)
	require.Equal(t, 1, friendNotices(t, db, b, NotificationCodeFriendRemove))
	require.NoError(t, DeleteFriends(ctx, logger, db, tr, nil, a, "fixture", []string{b.String()}))
	require.Equal(t, 1, friendNotices(t, db, b, NotificationCodeFriendRemove))
}

func TestDualBackendFriendLists(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	owner, _ := lifecycleUser(t, db)
	peers := []uuid.UUID{}
	for state := 0; state < 4; state++ {
		for i := 0; i < 2; i++ {
			peer, _ := lifecycleUser(t, db)
			peers = append(peers, peer)
			switch state {
			case 0:
				friendAccept(t, db, owner, peer)
			case 1:
				friendAdd(t, db, owner, peer, "")
			case 2:
				friendAdd(t, db, peer, owner, "")
			case 3:
				require.NoError(t, BlockFriends(ctx, logger, db, &friendTracker{}, owner, []string{peer.String()}))
			}
		}
	}
	for _, filter := range []*wrapperspb.Int32Value{nil, wrapperspb.Int32(0), wrapperspb.Int32(1), wrapperspb.Int32(2), wrapperspb.Int32(3)} {
		full, err := ListFriends(ctx, logger, db, nil, owner, 0, filter, "")
		require.NoError(t, err)
		if filter == nil {
			require.Len(t, full.Friends, 8)
		} else {
			require.Len(t, full.Friends, 2)
		}
		cursor := ""
		seen := []string{}
		for page := 0; page < 10; page++ {
			list, err := ListFriends(ctx, logger, db, nil, owner, 1, filter, cursor)
			require.NoError(t, err)
			for _, f := range list.Friends {
				seen = append(seen, f.User.Id)
				if filter != nil {
					require.Equal(t, filter.Value, f.State.Value)
				}
			}
			cursor = list.Cursor
			if cursor == "" {
				break
			}
		}
		require.Empty(t, cursor)
		expected := []string{}
		for _, f := range full.Friends {
			expected = append(expected, f.User.Id)
		}
		require.Equal(t, expected, seen)
	}
	ids, err := GetFriendIDs(ctx, logger, db, owner)
	require.NoError(t, err)
	require.Len(t, ids.Friends, 8)
	_, err = ListFriends(ctx, logger, db, nil, owner, 2, nil, "invalid!")
	require.ErrorIs(t, err, runtime.ErrFriendInvalidCursor)
	first, err := ListFriends(ctx, logger, db, nil, owner, 1, wrapperspb.Int32(0), "")
	require.NoError(t, err)
	_, err = ListFriends(ctx, logger, db, nil, owner, 1, wrapperspb.Int32(1), first.Cursor)
	require.ErrorIs(t, err, runtime.ErrFriendInvalidCursor)
	peer, _ := lifecycleUser(t, db)
	friendAccept(t, db, peers[0], peer)
	friendAccept(t, db, peers[1], peer)
	fof, err := ListFriendsOfFriends(ctx, logger, db, lifecycleStatus{}, owner, 100, "")
	require.NoError(t, err)
	require.Len(t, fof.FriendsOfFriends, 2)
	for _, f := range fof.FriendsOfFriends {
		require.Equal(t, peer.String(), f.User.Id)
	}
	friendCounts(t, db, owner)
}

func TestDualBackendFriendsOfFriendsPaginationAndBlocks(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	owner, _ := lifecycleUser(t, db)
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	c, _ := lifecycleUser(t, db)
	d, _ := lifecycleUser(t, db)
	friendAccept(t, db, owner, a)
	friendAccept(t, db, owner, b)
	friendAccept(t, db, a, c)
	friendAccept(t, db, b, d)
	first, err := ListFriendsOfFriends(ctx, logger, db, lifecycleStatus{}, owner, 1, "")
	require.NoError(t, err)
	require.Len(t, first.FriendsOfFriends, 1)
	require.NotEmpty(t, first.Cursor)
	next, err := ListFriendsOfFriends(ctx, logger, db, lifecycleStatus{}, owner, 1, first.Cursor)
	require.NoError(t, err)
	require.Len(t, next.FriendsOfFriends, 1)
	require.Empty(t, next.Cursor)
	require.ElementsMatch(t, []string{c.String(), d.String()}, []string{first.FriendsOfFriends[0].User.Id, next.FriendsOfFriends[0].User.Id})
	require.NoError(t, BlockFriends(ctx, logger, db, &friendTracker{}, owner, []string{c.String()}))
	require.NoError(t, BlockFriends(ctx, logger, db, &friendTracker{}, d, []string{owner.String()}))
	blocked, err := ListFriendsOfFriends(ctx, logger, db, lifecycleStatus{}, owner, 100, "")
	require.NoError(t, err)
	require.Empty(t, blocked.FriendsOfFriends)
	_, err = ListFriendsOfFriends(ctx, logger, db, lifecycleStatus{}, owner, 1, "invalid!")
	require.ErrorIs(t, err, runtime.ErrFriendInvalidCursor)
}

func TestDualBackendFriendRollback(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	c, _ := lifecycleUser(t, db)
	peers := []string{b.String(), c.String()}
	sort.Strings(peers)
	dropFailure := failureTrigger(t, db, "user_edge", "INSERT", "NEW.source_id = '"+a.String()+"' AND NEW.destination_id = '"+peers[1]+"'")
	require.Error(t, AddFriends(ctx, zap.NewNop(), db, &friendTracker{}, nil, a, "fixture", peers, "{}"))
	friendState(t, db, a, b, -1)
	friendState(t, db, a, c, -1)
	friendCounts(t, db, a, b, c)
	require.Zero(t, friendNotices(t, db, b, NotificationCodeFriendRequest))
	require.Zero(t, friendNotices(t, db, c, NotificationCodeFriendRequest))
	tr := &friendTracker{}
	require.Error(t, BlockFriends(ctx, zap.NewNop(), db, tr, a, peers))
	require.Empty(t, tr.untracked)
	friendCounts(t, db, a, b, c)
	dropFailure()
	tr.check = func() { friendState(t, db, a, b, 3); friendState(t, db, a, c, 3) }
	require.NoError(t, BlockFriends(ctx, logger, db, tr, a, peers))
	require.Len(t, tr.untracked, 2)
	require.Error(t, AddFriends(ctx, zap.NewNop(), db, tr, nil, a, "fixture", []string{b.String()}, "invalid JSON"))
	require.Error(t, AddFriends(ctx, zap.NewNop(), db, tr, nil, a, "fixture", []string{"not a UUID"}, "{}"))
	require.NoError(t, AddFriends(ctx, logger, db, tr, nil, a, "fixture", []string{a.String(), uuid.Nil.String()}, "{}"))
	friendCounts(t, db, a, b, c)
}

func TestDualBackendFriendRacesAndDeletion(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	gate := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			owner, peer := a, b
			if i%2 == 1 {
				owner, peer = b, a
			}
			errs <- AddFriends(ctx, logger, db, &friendTracker{}, nil, owner, "fixture", []string{peer.String()}, "{}")
		}(i)
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	friendState(t, db, a, b, 0)
	friendState(t, db, b, a, 0)
	friendCounts(t, db, a, b)
	require.Equal(t, 2, lifecycleCount(t, db, "SELECT COUNT(*) FROM notification WHERE user_id IN (?,?) AND code IN (-2,-3)", a, b))
	// Block and a reverse add can complete in either order, but the block wins.
	errs = make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- BlockFriends(ctx, zap.NewNop(), db, &friendTracker{}, a, []string{b.String()})
	}()
	go func() {
		defer wg.Done()
		errs <- AddFriends(ctx, logger, db, &friendTracker{}, nil, b, "fixture", []string{a.String()}, "{}")
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	friendState(t, db, a, b, 3)
	friendState(t, db, b, a, -1)
	friendCounts(t, db, a, b)
	require.NoError(t, DeleteFriends(ctx, logger, db, &friendTracker{}, nil, a, "fixture", []string{b.String()}))
	cache := NewLocalSessionCache(60, 600)
	t.Cleanup(cache.Stop)
	errs = make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- DeleteAccount(ctx, zap.NewNop(), db, nil, nil, &lifecycleRanks{}, &lifecycleSessions{}, cache, lifecycleTracker{}, a, false, nil)
	}()
	go func() {
		defer wg.Done()
		errs <- AddFriends(ctx, logger, db, &friendTracker{}, nil, b, "fixture", []string{a.String()}, "{}")
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	friendState(t, db, b, a, -1)
	friendCounts(t, db, b)
	require.ErrorIs(t, AddFriends(ctx, logger, db, &friendTracker{}, nil, a, "fixture", []string{b.String()}, "{}"), ErrAccountNotFound)
}

func importFriendsFixture(t *testing.T, h *providerHarness, db *sql.DB, owner uuid.UUID, provider string, ids []string, reset bool) error {
	t.Helper()
	h.mu.Lock()
	if provider == "facebook" {
		h.facebookFriends[owner.String()] = [][]string{ids}
	} else {
		h.steamFriends[owner.String()] = ids
	}
	h.mu.Unlock()
	if provider == "facebook" {
		return importFacebookFriends(context.Background(), h.logger, db, &friendTracker{}, nil, h.client, owner, "fixture", owner.String(), reset)
	}
	return importSteamFriends(context.Background(), h.logger, db, &friendTracker{}, nil, h.client, owner, "fixture", "fixture", owner.String(), reset)
}
func TestDualBackendProviderFriendImport(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	h := newProviderHarness(t)
	for _, provider := range []string{"facebook", "steam"} {
		t.Run(provider, func(t *testing.T) {
			owner, _ := lifecycleUser(t, db)
			a, _ := lifecycleUser(t, db)
			b, _ := lifecycleUser(t, db)
			old, _ := lifecycleUser(t, db)
			blocked, _ := lifecycleUser(t, db)
			// Provider IDs need only be unique. Steam's wire fixture requires uint64.
			idA, idB := fmtSteamFixtureID(a), fmtSteamFixtureID(b)
			column := provider + "_id"
			lifecycleExec(t, db, "UPDATE users SET "+column+" = ? WHERE id = ?", idA, a)
			lifecycleExec(t, db, "UPDATE users SET "+column+" = ? WHERE id = ?", idB, b)
			friendAccept(t, db, owner, old)
			friendAdd(t, db, a, owner, "")
			require.NoError(t, BlockFriends(ctx, logger, db, &friendTracker{}, owner, []string{blocked.String()}))
			require.NoError(t, importFriendsFixture(t, h, db, owner, provider, []string{idA, idA, idB, "999999999999999999"}, false))
			friendState(t, db, owner, a, 0)
			friendState(t, db, owner, b, 0)
			friendState(t, db, owner, old, 0)
			friendState(t, db, owner, blocked, 3)
			friendCounts(t, db, owner, a, b, old, blocked)
			imported, err := GetFriends(ctx, logger, db, nil, owner, []uuid.UUID{a, b})
			require.NoError(t, err)
			for _, f := range imported {
				require.JSONEq(t, `{"provider":"`+provider+`"}`, f.Metadata)
			}
			require.Equal(t, 1, friendNotices(t, db, a, NotificationCodeFriendJoinGame))
			require.Equal(t, 1, friendNotices(t, db, b, NotificationCodeFriendJoinGame))
			require.NoError(t, importFriendsFixture(t, h, db, owner, provider, []string{idA, idB}, false))
			require.Equal(t, 1, friendNotices(t, db, a, NotificationCodeFriendJoinGame))
			require.NoError(t, importFriendsFixture(t, h, db, owner, provider, []string{idB}, true))
			friendState(t, db, owner, old, -1)
			friendState(t, db, old, owner, -1)
			friendState(t, db, owner, a, -1)
			friendState(t, db, owner, b, 0)
			friendState(t, db, owner, blocked, 3)
			friendCounts(t, db, owner, a, b, old, blocked)
			require.NoError(t, importFriendsFixture(t, h, db, owner, provider, nil, true))
			friendState(t, db, owner, b, -1)
			friendState(t, db, owner, blocked, 3)
			friendCounts(t, db, owner, a, b, old, blocked)
		})
	}
}
func fmtSteamFixtureID(id uuid.UUID) string {
	var n uint64
	for _, b := range id.Bytes()[:7] {
		n = n<<8 | uint64(b)
	}
	return fmt.Sprint(n)
}

func TestDualBackendProviderFriendsBlocksRollbackAndPages(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	h := newProviderHarness(t)
	owner, _ := lifecycleUser(t, db)
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	old, _ := lifecycleUser(t, db)
	idA, idB := GenerateString(), GenerateString()
	lifecycleExec(t, db, "UPDATE users SET facebook_id = ? WHERE id = ?", idA, a)
	lifecycleExec(t, db, "UPDATE users SET facebook_id = ? WHERE id = ?", idB, b)
	friendAccept(t, db, owner, old)
	h.facebookFriends[owner.String()] = [][]string{{idA}, {idB}}
	require.NoError(t, importFacebookFriends(ctx, h.logger, db, &friendTracker{}, nil, h.client, owner, "fixture", owner.String(), false))
	friendState(t, db, owner, a, 0)
	friendState(t, db, owner, b, 0)
	require.NoError(t, BlockFriends(ctx, logger, db, &friendTracker{}, owner, []string{a.String()}))
	require.NoError(t, BlockFriends(ctx, logger, db, &friendTracker{}, b, []string{owner.String()}))
	require.NoError(t, importFriendsFixture(t, h, db, owner, "facebook", []string{idA, idB}, false))
	friendState(t, db, owner, a, 3)
	friendState(t, db, a, owner, -1)
	friendState(t, db, b, owner, 3)
	friendState(t, db, owner, b, -1)
	friendCounts(t, db, owner, a, b)
	// A second-page provider failure must not reset or partially import anything.
	h.facebookFriends[owner.String()] = [][]string{{idA}, {idB}}
	h.friendErrors[owner.String()+":1"] = 500
	err := importFacebookFriends(ctx, h.logger, db, &friendTracker{}, nil, h.client, owner, "fixture", owner.String(), true)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	friendState(t, db, owner, old, 0)
	delete(h.friendErrors, owner.String()+":1")
	require.NoError(t, DeleteFriends(ctx, logger, db, &friendTracker{}, nil, owner, "fixture", []string{a.String()}))
	_ = failureTrigger(t, db, "user_edge", "INSERT", "NEW.source_id = '"+owner.String()+"' AND NEW.destination_id = '"+a.String()+"'")
	require.Equal(t, codes.Internal, status.Code(importFriendsFixture(t, h, db, owner, "facebook", []string{idA}, true)))
	friendState(t, db, owner, old, 0)
	friendState(t, db, owner, a, -1)
	friendState(t, db, b, owner, 3)
	friendCounts(t, db, owner, a, b, old)
	require.Equal(t, 1, friendNotices(t, db, a, NotificationCodeFriendJoinGame))
	// Steam private friend lists are an upstream no-op unless reset is explicit.
	h.friendErrors[owner.String()] = 401
	require.NoError(t, importSteamFriends(ctx, h.logger, db, &friendTracker{}, nil, h.client, owner, "fixture", "fixture", owner.String(), false))
	friendState(t, db, owner, old, 0)
}

func TestDualBackendFriendMetadataConcatenation(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	for _, tc := range []struct{ left, right, want string }{
		{`{"a":1,"nested":{"x":1}}`, `{"a":null,"nested":{"y":2}}`, `{"a":null,"nested":{"y":2}}`},
		{`[1,2]`, `[3]`, `[1,2,3]`},
		{`{"a":1}`, `[2]`, `[{"a":1},2]`},
		{`null`, `{"a":1}`, `[null,{"a":1}]`},
		{`true`, `7`, `[true,7]`},
	} {
		t.Run(tc.left+"+"+tc.right, func(t *testing.T) {
			a, _ := lifecycleUser(t, db)
			b, _ := lifecycleUser(t, db)
			friendAdd(t, db, a, b, "")
			lifecycleExec(t, db, "UPDATE user_edge SET metadata = ? WHERE source_id = ? AND destination_id = ?", tc.left, b, a)
			friendAdd(t, db, b, a, tc.right)
			friends, err := GetFriends(ctx, logger, db, nil, b, []uuid.UUID{a})
			require.NoError(t, err)
			require.Len(t, friends, 1)
			require.JSONEq(t, tc.want, friends[0].Metadata)
		})
	}
}

func TestDualBackendProviderFriendSyncOnLink(t *testing.T) {
	db, ctx := identityTestDB(t), context.Background()
	h := newProviderHarness(t)
	for _, kind := range []string{"facebook_classic", "steam"} {
		t.Run(kind, func(t *testing.T) {
			owner, _ := lifecycleUser(t, db)
			peer, _ := lifecycleUser(t, db)
			proof := newProviderProof(t, h, kind)
			peerID := fmtSteamFixtureID(peer)
			lifecycleExec(t, db, "UPDATE users SET "+proof.profile.column+" = ? WHERE id = ?", peerID, peer)
			if kind == "facebook_classic" {
				h.facebookFriends[proof.token] = [][]string{{peerID}}
				require.NoError(t, LinkFacebook(ctx, h.logger, db, h.client, &friendTracker{}, nil, owner, "fixture", "fixture", proof.token, true))
			} else {
				h.steamFriends[proof.profile.id] = []string{peerID}
				require.NoError(t, LinkSteam(ctx, h.logger, db, h.config, h.client, &friendTracker{}, nil, owner, "fixture", proof.token, true))
			}
			friendState(t, db, owner, peer, 0)
			friendState(t, db, peer, owner, 0)
			friendCounts(t, db, owner, peer)
			require.Equal(t, 1, friendNotices(t, db, peer, NotificationCodeFriendJoinGame))
		})
	}
}
