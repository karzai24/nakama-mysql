// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

func TestDualBackendRankTransactionHelpers(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx := context.Background()
	b := rankTestTournament(t, db, cache, true, 2, 2)
	owner := uuid.Must(uuid.NewV4())
	join := tournamentJoinMySQL
	write := leaderboardRecordWriteMySQL
	remove := leaderboardRecordDeleteMySQL
	attempt := tournamentAddAttemptMySQL
	callback := tournamentCallbackMySQL
	{
		join = tournamentJoinMySQL
		write = leaderboardRecordWriteMySQL
		remove = leaderboardRecordDeleteMySQL
		attempt = tournamentAddAttemptMySQL
		callback = tournamentCallbackMySQL
	}
	require.NoError(t, join(ctx, db, ranks, owner, "player", b.Id))
	start := make(chan struct{})
	results := make(chan error, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := write(ctx, db, ranks, uuid.Nil, b.Id, owner, "", 1, 0, "{}", api.Operator_INCREMENT, true)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, runtime.ErrTournamentWriteMaxNumScoreReached)
		}
	}
	require.Equal(t, 2, successes)
	require.Error(t, attempt(ctx, db, b.Id, owner.String(), -3))
	require.NoError(t, attempt(ctx, db, b.Id, owner.String(), 1))
	record, err := write(ctx, db, ranks, uuid.Nil, b.Id, owner, "", 1, 0, "{}", api.Operator_INCREMENT, true)
	require.NoError(t, err)
	require.Equal(t, int64(3), record.Score)
	tour, err := callback(ctx, db, b.Id, time.Now().UTC(), true)
	require.NoError(t, err)
	require.Equal(t, uint32(1), tour.Size)
	require.NoError(t, remove(ctx, db, ranks, uuid.Nil, b.Id, owner.String(), true))
	require.Zero(t, lifecycleCount(t, db, "SELECT size FROM leaderboard WHERE id = ?", b.Id))
}
