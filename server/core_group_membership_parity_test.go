// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/stretchr/testify/require"
)

func membershipParityCall(db *sql.DB, router *groupRouter, group, user uuid.UUID, name string, join bool) error {

	if join {
		return JoinGroup(context.Background(), logger, db, &groupTracker{}, router, group, user, name)
	}
	return LeaveGroup(context.Background(), logger, db, &groupTracker{}, router, &groupManager{}, group, user, name)
}

func TestDualBackendGroupMembershipRaces(t *testing.T) {
	db := identityTestDB(t)
	for _, open := range []bool{true, false} {
		t.Run(fmt.Sprint(open), func(t *testing.T) {
			owner, _ := lifecycleUser(t, db)
			member, name := lifecycleUser(t, db)
			group := groupFixture(t, db, owner, open, 4)
			router := &groupRouter{}
			run := func(join bool) {
				start := make(chan struct{})
				results := make(chan error, 8)
				for i := 0; i < 8; i++ {
					go func() { <-start; results <- membershipParityCall(db, router, group, member, name, join) }()
				}
				close(start)
				for i := 0; i < 8; i++ {
					require.NoError(t, <-results)
				}
			}
			run(true)
			state, count, messages := 3, 1, 0
			if open {
				state, count, messages = 2, 2, 1
			}
			groupEdge(t, db, group, member, state)
			groupCount(t, db, group, count)
			require.Len(t, router.messages, messages)
			if !open {
				require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM notification WHERE user_id = ? AND sender_id = ? AND code = ?", owner, member, NotificationCodeGroupJoinRequest))
			}
			router.messages = nil
			run(false)
			groupEdge(t, db, group, member, -1)
			groupCount(t, db, group, 1)
			require.Len(t, router.messages, 1)
		})
	}
}

func TestDualBackendGroupLeaveLastAdminRace(t *testing.T) {
	db := identityTestDB(t)
	first, firstName := lifecycleUser(t, db)
	second, secondName := lifecycleUser(t, db)
	member, _ := lifecycleUser(t, db)
	group := groupFixture(t, db, first, true, 4)
	groupAdd(t, db, group, first, second, member)
	groupRole(t, db, group, first, second, true)
	groupRole(t, db, group, first, second, true)
	groupEdge(t, db, group, first, 0)
	groupEdge(t, db, group, second, 0)
	groupCount(t, db, group, 3)
	start := make(chan struct{})
	results := make(chan error, 2)
	router := &groupRouter{}
	for _, user := range []struct {
		id   uuid.UUID
		name string
	}{{first, firstName}, {second, secondName}} {
		go func() { <-start; results <- membershipParityCall(db, router, group, user.id, user.name, false) }()
	}
	close(start)
	success, denied := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else {
			require.True(t, errors.Is(err, runtime.ErrGroupLastSuperadmin), "%v", err)
			denied++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, denied)
	groupCount(t, db, group, 2)
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? AND state = 0", group))
	require.Len(t, router.messages, 1)
}

func TestDualBackendGroupJoinRollback(t *testing.T) {
	db := identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	member, name := lifecycleUser(t, db)
	group := groupFixture(t, db, owner, true, 4)
	drop := failureTrigger(t, db, "message", "INSERT", "NEW.stream_subject = '"+group.String()+"'")
	router := &groupRouter{}
	require.Error(t, membershipParityCall(db, router, group, member, name, true))
	groupEdge(t, db, group, member, -1)
	groupCount(t, db, group, 1)
	require.Empty(t, router.messages)
	drop()
	require.NoError(t, membershipParityCall(db, router, group, member, name, true))
	groupCount(t, db, group, 2)
	require.Len(t, router.messages, 1)
}

func TestDualBackendGroupDisabledMembership(t *testing.T) {
	db := identityTestDB(t)
	owner, name := lifecycleUser(t, db)
	group := groupFixture(t, db, owner, true, 4)
	lifecycleExec(t, db, "UPDATE `groups` SET disable_time = ? WHERE id = ?", time.Now().UTC(), group)
	require.ErrorIs(t, membershipParityCall(db, &groupRouter{}, group, owner, name, true), runtime.ErrGroupNotFound)
	require.ErrorIs(t, membershipParityCall(db, &groupRouter{}, group, owner, name, false), runtime.ErrGroupNotFound)
	groupCount(t, db, group, 1)
	require.NoError(t, DeleteGroup(context.Background(), logger, db, &groupTracker{}, group, uuid.Nil))
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM `groups` WHERE id = ?", group))
	require.NoError(t, membershipParityCall(db, &groupRouter{}, group, owner, name, false))
}
