// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDualBackendConsoleGroupRoleRaces(t *testing.T) {
	s, ctx := consoleFixture(t)
	owner, _ := lifecycleUser(t, s.db)
	second, _ := lifecycleUser(t, s.db)
	group := groupFixture(t, s.db, owner, true, 4)
	groupAdd(t, s.db, group, owner, second)
	groupRole(t, s.db, group, owner, second, true)
	groupRole(t, s.db, group, owner, second, true)
	router := s.router.(*groupRouter)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []uuid.UUID{owner, second} {
		go func() {
			<-start
			_, err := s.DemoteGroupMember(ctx, &console.UpdateGroupUserStateRequest{GroupId: group.String(), Id: id.String()})
			results <- err
		}()
	}
	close(start)
	success, denied := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else {
			require.Equal(t, codes.FailedPrecondition, status.Code(err))
			denied++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, denied)
	require.Len(t, router.messages, 1)
	require.Equal(t, 1, lifecycleCount(t, s.db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? AND state = 0", group))
	// A ban is not a pending request and cannot be promoted back into membership.
	banned, _ := lifecycleUser(t, s.db)
	groupAdd(t, s.db, group, uuid.Nil, banned)
	require.NoError(t, BanGroupUsers(ctx, s.logger, s.db, s.tracker, s.router, s.streamManager, uuid.Nil, group, []uuid.UUID{banned}))
	_, err := s.PromoteGroupMember(ctx, &console.UpdateGroupUserStateRequest{GroupId: group.String(), Id: banned.String()})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	groupEdge(t, s.db, group, banned, 4)
}

func TestDualBackendConsoleGroupAcceptCapacityRace(t *testing.T) {
	s, ctx := consoleFixture(t)
	owner, _ := lifecycleUser(t, s.db)
	group := groupFixture(t, s.db, owner, false, 2)
	first, firstName := lifecycleUser(t, s.db)
	second, secondName := lifecycleUser(t, s.db)
	for _, user := range []struct {
		id   uuid.UUID
		name string
	}{{first, firstName}, {second, secondName}} {
		require.NoError(t, JoinGroup(ctx, s.logger, s.db, s.tracker, s.router, group, user.id, user.name))
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []uuid.UUID{first, second} {
		go func() {
			<-start
			_, err := s.PromoteGroupMember(ctx, &console.UpdateGroupUserStateRequest{GroupId: group.String(), Id: id.String()})
			results <- err
		}()
	}
	close(start)
	success := 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else {
			require.Equal(t, codes.Internal, status.Code(err))
		}
	}
	require.Equal(t, 1, success)
	groupCount(t, s.db, group, 2)
	require.Equal(t, 1, lifecycleCount(t, s.db, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? AND state = 3", group))
	require.Len(t, s.router.(*groupRouter).messages, 1)
}
