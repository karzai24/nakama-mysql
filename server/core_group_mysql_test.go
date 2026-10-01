// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type groupTracker struct {
	friendTracker
	presences []*Presence
}

func (g *groupTracker) ListByStream(_ PresenceStream, _, _ bool) []*Presence { return g.presences }

type groupRouter struct {
	MessageRouter
	mu       sync.Mutex
	messages []*api.ChannelMessage
	check    func(*api.ChannelMessage)
}

func (r *groupRouter) SendToStream(_ *zap.Logger, _ PresenceStream, e *rtapi.Envelope, _ bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := e.GetChannelMessage()
	if r.check != nil {
		r.check(m)
	}
	r.messages = append(r.messages, m)
}

type groupManager struct {
	StreamManager
	left  []uuid.UUID
	check func()
}

func (m *groupManager) UserLeave(_ PresenceStream, uid, _ uuid.UUID) error {
	if m.check != nil {
		m.check()
	}
	m.left = append(m.left, uid)
	return nil
}

type groupStatus struct{ lifecycleStatus }

func (s groupStatus) FillOnlineGroupUsers(users []*api.GroupUserList_GroupUser) {
	for _, u := range users {
		u.User.Online = u.User.Id == s.online
	}
}

func groupFixture(t *testing.T, db *sql.DB, owner uuid.UUID, open bool, max int) uuid.UUID {
	t.Helper()
	g, err := CreateGroup(context.Background(), logger, db, owner, owner, "group_"+GenerateString(), "en", "description", "avatar", `{"large":9007199254740993,"nested":{"label":"فارسی 🂡"}}`, open, max)
	require.NoError(t, err)
	id := uuid.Must(uuid.FromString(g.Id))
	t.Cleanup(func() {
		lifecycleExec(t, db, "DELETE FROM message WHERE stream_subject = ?", id)
		lifecycleExec(t, db, "DELETE FROM group_edge WHERE source_id = ? OR destination_id = ?", id, id)
		lifecycleExec(t, db, "DELETE FROM `groups` WHERE id = ?", id)
	})
	return id
}
func groupEdge(t *testing.T, db *sql.DB, g, u uuid.UUID, want int) {
	t.Helper()
	var state int
	err := db.QueryRow(lifecycleSQL(db, "SELECT state FROM group_edge WHERE source_id = ? AND destination_id = ?"), g, u).Scan(&state)
	if want < 0 {
		require.ErrorIs(t, err, sql.ErrNoRows)
	} else {
		require.NoError(t, err)
		require.Equal(t, want, state)
	}
	reverse := lifecycleCount(t, db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? AND destination_id = ?", u, g)
	if want < 0 || want == 4 {
		require.Zero(t, reverse)
	} else {
		require.Equal(t, 1, reverse)
		require.Equal(t, want, lifecycleCount(t, db, "SELECT state FROM group_edge WHERE source_id = ? AND destination_id = ?", u, g))
	}
}
func groupCount(t *testing.T, db *sql.DB, gid uuid.UUID, want int) {
	t.Helper()
	require.Equal(t, want, lifecycleCount(t, db, "SELECT edge_count FROM `groups` WHERE id = ?", gid))
	require.Equal(t, want, lifecycleCount(t, db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? AND state < 3", gid))
}
func groupAdd(t *testing.T, db *sql.DB, gid, caller uuid.UUID, ids ...uuid.UUID) {
	t.Helper()
	require.NoError(t, AddGroupUsers(context.Background(), logger, db, &groupTracker{}, &groupRouter{}, caller, gid, ids))
}
func groupRole(t *testing.T, db *sql.DB, gid, caller, uid uuid.UUID, promote bool) {
	t.Helper()
	var err error
	if promote {
		err = PromoteGroupUsers(context.Background(), logger, db, &groupRouter{}, caller, gid, []uuid.UUID{uid})
	} else {
		err = DemoteGroupUsers(context.Background(), logger, db, &groupRouter{}, caller, gid, []uuid.UUID{uid})
	}
	require.NoError(t, err)
}

func TestDualBackendGroupCRUD(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	other, _ := lifecycleUser(t, db)
	gid := groupFixture(t, db, owner, true, 5)
	g, err := getGroup(ctx, logger, db, gid)
	require.NoError(t, err)
	require.True(t, g.Open.Value)
	require.EqualValues(t, 5, g.MaxCount)
	groupCount(t, db, gid, 1)
	groupEdge(t, db, gid, owner, 0)
	require.JSONEq(t, `{"large":9007199254740993,"nested":{"label":"فارسی 🂡"}}`, g.Metadata)
	_, err = CreateGroup(ctx, logger, db, owner, owner, g.Name, "", "", "", "", false, 0)
	require.ErrorIs(t, err, runtime.ErrGroupNameInUse)
	_, err = CreateGroup(ctx, logger, db, uuid.Nil, owner, GenerateString(), "", "", "", "", false, 0)
	require.ErrorIs(t, err, runtime.ErrGroupCreatorInvalid)
	require.ErrorIs(t, UpdateGroup(ctx, logger, db, gid, other, uuid.Nil, nil, nil, nil, nil, nil, nil, 0), runtime.ErrGroupPermissionDenied)
	require.ErrorIs(t, UpdateGroup(ctx, logger, db, gid, owner, uuid.Nil, nil, nil, nil, nil, nil, nil, 0), runtime.ErrGroupNoUpdateOps)
	name := "updated_" + GenerateString()
	require.NoError(t, UpdateGroup(ctx, logger, db, gid, owner, other, wrapperspb.String(name), wrapperspb.String("fa"), wrapperspb.String(""), wrapperspb.String(""), wrapperspb.String(`{"array":[null,9007199254740993]}`), wrapperspb.Bool(false), 7))
	g, err = getGroup(ctx, logger, db, gid)
	require.NoError(t, err)
	require.False(t, g.Open.Value)
	require.Empty(t, g.Description)
	require.Empty(t, g.AvatarUrl)
	require.Equal(t, other.String(), g.CreatorId)
	require.Equal(t, name, g.Name)
	require.EqualValues(t, 7, g.MaxCount)
	require.JSONEq(t, `{"array":[null,9007199254740993]}`, g.Metadata)
	got, err := GetGroups(ctx, logger, db, []string{strings.ToUpper(gid.String()), gid.String(), uuid.Must(uuid.NewV4()).String()})
	require.NoError(t, err)
	require.Len(t, got, 1)
	_, err = GetGroups(ctx, logger, db, []string{"invalid"})
	require.Error(t, err)
	_, err = getGroup(ctx, logger, db, uuid.Must(uuid.NewV4()))
	require.ErrorIs(t, err, runtime.ErrGroupNotFound)
	require.ErrorIs(t, DeleteGroup(ctx, logger, db, &groupTracker{}, gid, other), runtime.ErrGroupPermissionDenied)
	tr := &groupTracker{}
	tr.check = func() { require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM `groups` WHERE id = ?", gid)) }
	require.NoError(t, DeleteGroup(ctx, logger, db, tr, gid, owner))
	require.Len(t, tr.untracked, 1)
	groupEdge(t, db, gid, owner, -1)
	require.NoError(t, DeleteGroup(ctx, logger, db, tr, gid, uuid.Nil))
}

func TestDualBackendGroupMembershipLifecycle(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	member, _ := lifecycleUser(t, db)
	admin, _ := lifecycleUser(t, db)
	gid := groupFixture(t, db, owner, false, 5)
	tr, r, m := &groupTracker{}, &groupRouter{}, &groupManager{}
	r.check = func(msg *api.ChannelMessage) {
		require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM message WHERE id = ?", msg.MessageId))
		require.Equal(t, gid.String(), msg.GroupId)
		require.True(t, msg.Persistent.Value)
	}
	require.NoError(t, JoinGroup(ctx, logger, db, tr, r, gid, member, "member"))
	groupEdge(t, db, gid, member, 3)
	groupCount(t, db, gid, 1)
	require.Equal(t, 1, friendNotices(t, db, owner, NotificationCodeGroupJoinRequest))
	require.Empty(t, r.messages)
	require.NoError(t, JoinGroup(ctx, logger, db, tr, r, gid, member, "member"))
	require.Equal(t, 1, friendNotices(t, db, owner, NotificationCodeGroupJoinRequest))
	require.ErrorIs(t, AddGroupUsers(ctx, logger, db, tr, r, member, gid, []uuid.UUID{admin}), runtime.ErrGroupPermissionDenied)
	require.NoError(t, AddGroupUsers(ctx, logger, db, tr, r, owner, gid, []uuid.UUID{member, member, admin}))
	groupEdge(t, db, gid, member, 2)
	groupCount(t, db, gid, 3)
	require.Len(t, r.messages, 2)
	require.Equal(t, 1, friendNotices(t, db, member, NotificationCodeGroupAdd))
	groupRole(t, db, gid, owner, admin, true)
	groupEdge(t, db, gid, admin, 1)
	// Admin cannot kick/promote the owner, but can promote a regular member.
	require.NoError(t, KickGroupUsers(ctx, logger, db, tr, r, m, admin, gid, []uuid.UUID{owner}, false))
	groupEdge(t, db, gid, owner, 0)
	groupRole(t, db, gid, admin, owner, true)
	groupEdge(t, db, gid, owner, 0)
	groupRole(t, db, gid, admin, member, true)
	groupEdge(t, db, gid, member, 1)
	require.NoError(t, BanGroupUsers(ctx, logger, db, tr, r, m, admin, gid, []uuid.UUID{member}))
	groupEdge(t, db, gid, member, 1)
	// Upstream allows an admin to demote a peer admin.
	groupRole(t, db, gid, admin, member, false)
	groupEdge(t, db, gid, member, 2)
	tr.presences = []*Presence{{UserID: member, ID: PresenceID{SessionID: uuid.Must(uuid.NewV4())}}}
	m.check = func() { groupEdge(t, db, gid, member, 4) }
	require.NoError(t, BanGroupUsers(ctx, logger, db, tr, r, m, admin, gid, []uuid.UUID{member}))
	groupEdge(t, db, gid, member, 4)
	groupCount(t, db, gid, 2)
	require.Equal(t, []uuid.UUID{member}, m.left)
	require.NoError(t, JoinGroup(ctx, logger, db, tr, r, gid, member, "member"))
	groupEdge(t, db, gid, member, 4)
	require.NoError(t, AddGroupUsers(ctx, logger, db, tr, r, owner, gid, []uuid.UUID{member}))
	groupEdge(t, db, gid, member, 4)
	require.NoError(t, LeaveGroup(ctx, logger, db, tr, r, m, gid, member, "member"))
	groupEdge(t, db, gid, member, 4)
	require.NoError(t, KickGroupUsers(ctx, logger, db, tr, r, m, owner, gid, []uuid.UUID{member}, true))
	groupEdge(t, db, gid, member, -1)
	require.ErrorIs(t, KickGroupUsers(ctx, logger, db, tr, r, m, owner, gid, []uuid.UUID{member}, true), sql.ErrNoRows)
	require.ErrorIs(t, LeaveGroup(ctx, logger, db, tr, r, m, gid, owner, "owner"), runtime.ErrGroupLastSuperadmin)
	groupRole(t, db, gid, uuid.Nil, owner, false)
	groupEdge(t, db, gid, owner, 0)
	groupRole(t, db, gid, owner, admin, true)
	groupEdge(t, db, gid, admin, 0)
	m.check = nil
	require.NoError(t, LeaveGroup(ctx, logger, db, tr, r, m, gid, owner, "owner"))
	groupEdge(t, db, gid, owner, -1)
	groupCount(t, db, gid, 1)
	require.NoError(t, JoinGroup(ctx, logger, db, tr, r, gid, member, "member"))
	groupEdge(t, db, gid, member, 3)
	require.NoError(t, LeaveGroup(ctx, logger, db, tr, r, m, gid, member, "member"))
	groupEdge(t, db, gid, member, -1)
	groupCount(t, db, gid, 1)
	require.NoError(t, JoinGroup(ctx, logger, db, tr, r, gid, member, "member"))
	require.NoError(t, LeaveGroup(ctx, logger, db, tr, r, m, gid, admin, "admin"))
	require.Len(t, tr.untracked, 1)
	groupEdge(t, db, gid, member, -1)
}

func TestDualBackendGroupAtomicBatches(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	a, _ := lifecycleUser(t, db)
	b, _ := lifecycleUser(t, db)
	gid := groupFixture(t, db, owner, true, 2)
	tr, r, m := &groupTracker{}, &groupRouter{}, &groupManager{}
	require.ErrorIs(t, AddGroupUsers(ctx, logger, db, tr, r, owner, gid, []uuid.UUID{a, b}), runtime.ErrGroupFull)
	groupCount(t, db, gid, 1)
	groupEdge(t, db, gid, a, -1)
	groupEdge(t, db, gid, b, -1)
	require.Empty(t, r.messages)
	require.Zero(t, friendNotices(t, db, a, NotificationCodeGroupAdd))
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM message WHERE stream_subject = ?", gid))
	require.ErrorIs(t, AddGroupUsers(ctx, logger, db, tr, r, owner, gid, []uuid.UUID{a, uuid.Must(uuid.NewV4())}), runtime.ErrGroupUserNotFound)
	groupEdge(t, db, gid, a, -1)
	groupCount(t, db, gid, 1)
	// Fail after writing the membership/counter, while persisting the channel event.
	require.Error(t, JoinGroup(ctx, logger, db, tr, r, gid, a, strings.Repeat("x", 129)))
	groupEdge(t, db, gid, a, -1)
	groupCount(t, db, gid, 1)
	require.Empty(t, r.messages)
	require.NoError(t, JoinGroup(ctx, logger, db, tr, r, gid, a, "a"))
	groupCount(t, db, gid, 2)
	require.ErrorIs(t, JoinGroup(ctx, logger, db, tr, r, gid, b, "b"), runtime.ErrGroupFull)
	require.ErrorIs(t, KickGroupUsers(ctx, logger, db, tr, r, m, owner, gid, []uuid.UUID{a, b}, true), sql.ErrNoRows)
	groupEdge(t, db, gid, a, 2)
	groupCount(t, db, gid, 2)
	require.Len(t, r.messages, 1)
	require.Error(t, UpdateGroup(ctx, logger, db, gid, owner, uuid.Nil, nil, nil, nil, nil, nil, nil, 1))
	groupCount(t, db, gid, 2)
}

func TestDualBackendGroupMembershipLists(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	gid := groupFixture(t, db, owner, false, 10)
	users := []uuid.UUID{owner}
	for i := 1; i <= 4; i++ {
		uid, _ := lifecycleUser(t, db)
		users = append(users, uid)
		require.NoError(t, JoinGroup(ctx, logger, db, &groupTracker{}, &groupRouter{}, gid, uid, "fixture"))
	}
	groupAdd(t, db, gid, owner, users[1], users[2], users[4])
	groupRole(t, db, gid, owner, users[1], true)
	require.NoError(t, BanGroupUsers(ctx, logger, db, &groupTracker{}, &groupRouter{}, &groupManager{}, owner, gid, []uuid.UUID{users[4]}))
	for _, filter := range []*wrapperspb.Int32Value{nil, wrapperspb.Int32(0), wrapperspb.Int32(1), wrapperspb.Int32(2), wrapperspb.Int32(3), wrapperspb.Int32(4)} {
		full, err := ListGroupUsers(ctx, logger, db, groupStatus{lifecycleStatus{online: owner.String()}}, gid, 0, filter, "")
		require.NoError(t, err)
		if filter == nil {
			require.Len(t, full.GroupUsers, 4)
		} else {
			require.Len(t, full.GroupUsers, 1)
		}
		cursor := ""
		got := []string{}
		for i := 0; i < 10; i++ {
			page, err := ListGroupUsers(ctx, logger, db, groupStatus{}, gid, 1, filter, cursor)
			require.NoError(t, err)
			for _, u := range page.GroupUsers {
				got = append(got, u.User.Id)
			}
			cursor = page.Cursor
			if cursor == "" {
				break
			}
		}
		want := []string{}
		for _, u := range full.GroupUsers {
			want = append(want, u.User.Id)
			if u.User.Id == owner.String() {
				require.True(t, u.User.Online)
			}
		}
		require.Equal(t, want, got)
	}
	first, err := ListGroupUsers(ctx, logger, db, groupStatus{}, gid, 1, nil, "")
	require.NoError(t, err)
	_, err = ListGroupUsers(ctx, logger, db, groupStatus{}, gid, 1, wrapperspb.Int32(0), first.Cursor)
	require.ErrorIs(t, err, runtime.ErrGroupUserInvalidCursor)
	_, err = ListGroupUsers(ctx, logger, db, groupStatus{}, gid, 1, nil, "bad")
	require.ErrorIs(t, err, runtime.ErrGroupUserInvalidCursor)
	// User-group pagination spans all roles; bans have no reverse edge.
	for i := 1; i < 4; i++ {
		g := groupFixture(t, db, users[i], false, 10)
		require.NoError(t, JoinGroup(ctx, logger, db, &groupTracker{}, &groupRouter{}, g, owner, "owner"))
		if i < 3 {
			groupAdd(t, db, g, users[i], owner)
			if i == 1 {
				groupRole(t, db, g, users[i], owner, true)
			}
		}
	}
	for _, filter := range []*wrapperspb.Int32Value{nil, wrapperspb.Int32(0), wrapperspb.Int32(1), wrapperspb.Int32(2), wrapperspb.Int32(3)} {
		full, err := ListUserGroups(ctx, logger, db, owner, 0, filter, "")
		require.NoError(t, err)
		cursor := ""
		got := []string{}
		for i := 0; i < 10; i++ {
			page, err := ListUserGroups(ctx, logger, db, owner, 1, filter, cursor)
			require.NoError(t, err)
			for _, g := range page.UserGroups {
				got = append(got, g.Group.Id)
			}
			cursor = page.Cursor
			if cursor == "" {
				break
			}
		}
		want := []string{}
		for _, g := range full.UserGroups {
			want = append(want, g.Group.Id)
		}
		require.Equal(t, want, got)
	}
	banned, err := ListUserGroups(ctx, logger, db, users[4], 0, nil, "")
	require.NoError(t, err)
	require.Empty(t, banned.UserGroups)
	_, err = ListUserGroups(ctx, logger, db, owner, 1, nil, "bad")
	require.ErrorIs(t, err, runtime.ErrUserGroupInvalidCursor)
	allowed, err := groupCheckUserPermission(ctx, logger, db, gid, users[1], 1)
	require.NoError(t, err)
	require.True(t, allowed)
	allowed, err = groupCheckUserPermission(ctx, logger, db, gid, users[4], 2)
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestDualBackendGroupDiscovery(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	prefix := "Search_" + GenerateString()
	lang := GenerateString()[:8]
	want := []string{}
	for i := 0; i < 4; i++ {
		gid := groupFixture(t, db, owner, i%2 == 0, 20)
		want = append(want, gid.String())
		require.NoError(t, UpdateGroup(ctx, logger, db, gid, owner, uuid.Nil, wrapperspb.String(fmt.Sprintf("%s_%d فارسی", prefix, i)), wrapperspb.String(lang), nil, nil, nil, nil, 0))
	}
	cursor := ""
	got := []string{}
	for i := 0; i < 8; i++ {
		page, err := ListGroups(ctx, logger, db, strings.ToLower(prefix)+"%", "", nil, -1, 1, cursor)
		require.NoError(t, err)
		for _, g := range page.Groups {
			got = append(got, g.Id)
		}
		cursor = page.Cursor
		if cursor == "" {
			break
		}
	}
	require.Equal(t, want, got)
	page, err := ListGroups(ctx, logger, db, "", lang, nil, -1, 20, "")
	require.NoError(t, err)
	require.Len(t, page.Groups, 4)
	_, err = ListGroups(ctx, logger, db, prefix, lang, nil, -1, 10, "")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, err.(*statusError).Code())
	_, err = ListGroups(ctx, logger, db, "", "", nil, -1, 10, "bad")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	random, err := GetRandomGroups(ctx, logger, db, 3)
	require.NoError(t, err)
	require.Len(t, random, 3)
	seen := map[string]bool{}
	for _, g := range random {
		require.False(t, seen[g.Id])
		seen[g.Id] = true
	}
	random, err = GetRandomGroups(ctx, logger, db, 0)
	require.NoError(t, err)
	require.Empty(t, random)
}

func TestDualBackendGroupDiscoveryFilters(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	lang := GenerateString()[:8]
	prefix := "filters_" + GenerateString()
	for i := 0; i < 6; i++ {
		g := groupFixture(t, db, owner, i%2 == 0, 10)
		require.NoError(t, UpdateGroup(ctx, logger, db, g, owner, uuid.Nil, wrapperspb.String(fmt.Sprintf("%s%d", prefix, i)), wrapperspb.String(lang), nil, nil, nil, nil, 0))
		if i%3 > 0 {
			user, _ := lifecycleUser(t, db)
			groupAdd(t, db, g, owner, user)
		}
	}
	open, closed := true, false
	for _, f := range []struct {
		lang  string
		open  *bool
		count int
	}{{lang, &open, 2}, {lang, &open, -1}, {lang, &closed, -1}, {"", &open, 2}, {lang, nil, 2}, {lang, nil, -1}, {"", nil, 2}, {"", &open, -1}, {"", nil, -1}} {
		full, err := ListGroups(ctx, logger, db, "", f.lang, f.open, f.count, 1000, "")
		require.NoError(t, err)
		expected := []string{}
		for _, g := range full.Groups {
			expected = append(expected, g.Id)
		}
		got := []string{}
		cursor := ""
		for i := 0; i < len(expected)+2; i++ {
			page, err := ListGroups(ctx, logger, db, "", f.lang, f.open, f.count, 1, cursor)
			require.NoError(t, err)
			for _, g := range page.Groups {
				got = append(got, g.Id)
			}
			cursor = page.Cursor
			if cursor == "" {
				break
			}
		}
		require.Empty(t, cursor)
		require.Equal(t, expected, got)
	}
	// Disabled groups are excluded even by the case-insensitive name lookup.
	g := groupFixture(t, db, owner, true, 3)
	lifecycleExec(t, db, "UPDATE `groups` SET name = ?, disable_time = ? WHERE id = ?", prefix+"disabled", time.Now().UTC(), g)
	page, err := ListGroups(ctx, logger, db, prefix+"disabled", "", nil, -1, 5, "")
	require.NoError(t, err)
	require.Empty(t, page.Groups)
	require.ErrorIs(t, JoinGroup(ctx, logger, db, &groupTracker{}, &groupRouter{}, g, owner, "owner"), runtime.ErrGroupNotFound)
}

func TestDualBackendGroupConcurrentMembership(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	gid := groupFixture(t, db, owner, true, 4)
	ids := []uuid.UUID{}
	for i := 0; i < 8; i++ {
		u, _ := lifecycleUser(t, db)
		ids = append(ids, u)
	}
	start := make(chan struct{})
	results := make(chan error, len(ids))
	router := &groupRouter{}
	for _, u := range ids {
		go func(uid uuid.UUID) {
			<-start
			results <- JoinGroup(ctx, logger, db, &groupTracker{}, router, gid, uid, "fixture")
		}(u)
	}
	close(start)
	successes := 0
	for range ids {
		err := <-results
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, runtime.ErrGroupFull)
		}
	}
	require.Equal(t, 3, successes)
	groupCount(t, db, gid, 4)
	require.Len(t, router.messages, 3)
	// Two authoritative demotions may race, but at least one superadmin must remain.
	group2 := groupFixture(t, db, owner, true, 4)
	groupAdd(t, db, group2, owner, ids[0])
	groupRole(t, db, group2, owner, ids[0], true)
	groupRole(t, db, group2, owner, ids[0], true)
	start = make(chan struct{})
	results = make(chan error, 2)
	for _, u := range []uuid.UUID{owner, ids[0]} {
		go func(uid uuid.UUID) {
			<-start
			results <- DemoteGroupUsers(ctx, logger, db, router, uuid.Nil, group2, []uuid.UUID{uid})
		}(u)
	}
	close(start)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? AND state = 0", group2))
	// Multi-user bans must retain every one-way edge (unique positions per pair).
	require.NoError(t, BanGroupUsers(ctx, logger, db, &groupTracker{}, router, &groupManager{}, owner, gid, ids))
	groupCount(t, db, gid, 1)
	require.Equal(t, 3, lifecycleCount(t, db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? AND state = 4", gid))
}

func TestDualBackendGroupAccountDeletionRace(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	gid := groupFixture(t, db, owner, true, 100)
	cache := NewLocalSessionCache(60, 60)
	defer cache.Stop()
	for i := 0; i < 8; i++ {
		uid, _ := lifecycleUser(t, db)
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			results <- AddGroupUsers(ctx, logger, db, &groupTracker{}, &groupRouter{}, owner, gid, []uuid.UUID{uid})
		}()
		go func() {
			<-start
			results <- DeleteAccount(ctx, logger, db, nil, nil, &lifecycleRanks{}, &lifecycleSessions{}, cache, lifecycleTracker{}, uid, false, nil)
		}()
		close(start)
		for j := 0; j < 2; j++ {
			err := <-results
			require.True(t, err == nil || errors.Is(err, runtime.ErrGroupUserNotFound), "%v", err)
		}
		groupEdge(t, db, gid, uid, -1)
		groupCount(t, db, gid, 1)
	}
}

func TestDualBackendGroupConcurrentDemotion(t *testing.T) {
	ctx, db := context.Background(), identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	peer, _ := lifecycleUser(t, db)
	gid := groupFixture(t, db, owner, true, 4)
	groupAdd(t, db, gid, owner, peer)
	groupRole(t, db, gid, owner, peer, true)
	groupRole(t, db, gid, owner, peer, true)
	start, results := make(chan struct{}), make(chan error, 2)
	router := &groupRouter{}
	for _, id := range []uuid.UUID{owner, peer} {
		go func(uid uuid.UUID) {
			<-start
			results <- DemoteGroupUsers(ctx, logger, db, router, uuid.Nil, gid, []uuid.UUID{uid})
		}(id)
	}
	close(start)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? AND state = 0", gid))
	require.Len(t, router.messages, 1)
}

func TestDualBackendGroupRejectMissingOwner(t *testing.T) {
	db := identityTestDB(t)
	missing := uuid.Must(uuid.NewV4())
	group, err := CreateGroup(context.Background(), logger, db, missing, missing, "missing_"+GenerateString(), "en", "", "", "{}", true, 4)
	require.ErrorIs(t, err, runtime.ErrGroupCreatorInvalid)
	require.Nil(t, group)
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM `groups` WHERE creator_id = ?", missing))
}
