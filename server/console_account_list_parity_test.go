// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"fmt"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sort"
	"testing"
)

func TestDualBackendConsoleAccountSearch(t *testing.T) {
	s, ctx := consoleFixture(t)
	suffix := GenerateString()
	ids := make([]string, 0, 105)
	for i := 0; i < 105; i++ {
		id := uuid.Must(uuid.NewV4()).String()
		ids = append(ids, id)
		lifecycleExec(t, s.db, "INSERT INTO users (id,username,display_name) VALUES (?,?,?)", id, fmt.Sprintf("search_%s_%03d", suffix, i), "Literal_%\\"+suffix)
		cleanupIdentityUser(t, s.db, id)
	}
	sort.Strings(ids)
	// Literal display-name matching never treats SQL wildcard characters as syntax.
	result, err := s.ListAccounts(ctx, &console.ListAccountsRequest{Filter: "_%\\" + suffix})
	require.NoError(t, err)
	require.Len(t, result.Users, 100)
	for i, user := range result.Users {
		require.Equal(t, ids[i], user.Id)
	}
	require.Empty(t, result.NextCursor)
	result, err = s.ListAccounts(ctx, &console.ListAccountsRequest{Filter: ids[0]})
	require.NoError(t, err)
	require.Len(t, result.Users, 1)
	require.Equal(t, ids[0], result.Users[0].Id)
	_, err = s.ListAccounts(ctx, &console.ListAccountsRequest{Cursor: "invalid cursor!"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	result, err = s.ListAccounts(ctx, &console.ListAccountsRequest{Filter: ids[0], Tombstones: true})
	require.NoError(t, err)
	require.Empty(t, result.Users)
	require.Zero(t, result.TotalCount)
	require.Equal(t, int32(lifecycleCount(t, s.db, "SELECT COUNT(*) FROM users")), countDatabase(ctx, s.logger, s.db, "users"))
	require.Zero(t, countDatabase(ctx, s.logger, s.db, "untrusted_table"))
}
