// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type rankTestScheduler struct{ LeaderboardScheduler }

func (rankTestScheduler) Update() {}
func rankTestCaches(t *testing.T) (*sql.DB, *LocalLeaderboardCache, *LocalLeaderboardRankCache) {
	t.Helper()
	db := identityTestDB(t)
	return db, &LocalLeaderboardCache{db: db, logger: zap.NewNop(), leaderboards: map[string]*Leaderboard{}}, &LocalLeaderboardRankCache{blacklistIds: map[string]struct{}{}, cache: map[LeaderboardWithExpiry]*RankCache{}}
}
func rankTestBoard(t *testing.T, db *sql.DB, cache *LocalLeaderboardCache, order, operator int) *Leaderboard {
	t.Helper()
	id := "rank_" + GenerateString()
	b, created, err := cache.Create(context.Background(), id, false, order, operator, "", `{"source":"contract"}`, true)
	require.NoError(t, err)
	require.True(t, created)
	t.Cleanup(func() { lifecycleExec(t, db, "DELETE FROM leaderboard WHERE id = ?", id) })
	return b
}
func rankTestTournament(t *testing.T, db *sql.DB, cache *LocalLeaderboardCache, join bool, maxSize, maxScore int) *Leaderboard {
	t.Helper()
	id := "tournament_" + GenerateString()
	b, created, err := cache.CreateTournament(context.Background(), id, false, LeaderboardSortOrderDescending, LeaderboardOperatorSet, "", `{"source":"contract"}`, "فارسی", "test", 1, int(time.Now().Add(-time.Minute).Unix()), 0, 3600, maxSize, maxScore, join, true)
	require.NoError(t, err)
	require.True(t, created)
	t.Cleanup(func() { lifecycleExec(t, db, "DELETE FROM leaderboard WHERE id = ?", id) })
	return b
}

func TestDualBackendLeaderboardOperators(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx := context.Background()
	for _, order := range []int{LeaderboardSortOrderAscending, LeaderboardSortOrderDescending} {
		for _, op := range []int{LeaderboardOperatorBest, LeaderboardOperatorSet, LeaderboardOperatorIncrement, LeaderboardOperatorDecrement} {
			t.Run(fmt.Sprintf("%d/%d", order, op), func(t *testing.T) {
				b := rankTestBoard(t, db, cache, order, op)
				owner := uuid.Must(uuid.NewV4()) // Virtual/team IDs are valid owners.
				write := func(score, sub int64, meta string) *api.LeaderboardRecord {
					r, err := LeaderboardRecordWrite(ctx, logger, db, cache, ranks, uuid.Nil, b.Id, strings.ToUpper(owner.String()), "player", score, sub, meta, api.Operator_NO_OVERRIDE)
					require.NoError(t, err)
					return r
				}
				first := write(10, 20, `{"old":true}`)
				require.Equal(t, int32(1), first.NumScore)
				second := write(5, 30, "")
				expected := map[int][2]int64{LeaderboardOperatorBest: {10, 30}, LeaderboardOperatorSet: {5, 30}, LeaderboardOperatorIncrement: {15, 50}, LeaderboardOperatorDecrement: {0, 0}}[op]
				if op == LeaderboardOperatorBest && order == LeaderboardSortOrderAscending {
					expected = [2]int64{5, 20}
				}
				require.Equal(t, expected[0], second.Score)
				require.Equal(t, expected[1], second.Subscore)
				require.Equal(t, int32(2), second.NumScore)
				require.JSONEq(t, `{"old":true}`, second.Metadata)
				require.Equal(t, int64(1), second.Rank)
				before := second
				sameScore, sameSub := second.Score, second.Subscore
				if op == LeaderboardOperatorIncrement || op == LeaderboardOperatorDecrement {
					sameScore, sameSub = 0, 0
				}
				unchanged := write(sameScore, sameSub, `{"ignored":true}`)
				require.Equal(t, before.NumScore, unchanged.NumScore)
				require.JSONEq(t, before.Metadata, unchanged.Metadata)
				_, err := LeaderboardRecordWrite(ctx, zap.NewNop(), db, cache, ranks, uuid.Nil, b.Id, owner.String(), "", 99, 99, "not json", api.Operator_SET)
				require.Error(t, err)
				page, err := LeaderboardRecordsList(ctx, logger, db, cache, ranks, b.Id, nil, "", []string{owner.String()}, 0)
				require.NoError(t, err)
				require.Equal(t, before.Score, page.OwnerRecords[0].Score)
				overridden, err := LeaderboardRecordWrite(ctx, logger, db, cache, ranks, uuid.Nil, b.Id, owner.String(), "", 42, 43, `{"new":true}`, api.Operator_SET)
				require.NoError(t, err)
				require.Equal(t, int64(42), overridden.Score)
				require.JSONEq(t, `{"new":true}`, overridden.Metadata)
				require.NoError(t, LeaderboardRecordDelete(ctx, logger, db, cache, ranks, uuid.Nil, b.Id, owner.String()))
				require.Zero(t, ranks.Get(b.Id, 0, owner))
			})
		}
	}
}

func TestDualBackendLeaderboardPaginationAndHaystack(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx := context.Background()
	for _, order := range []int{0, 1} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			b := rankTestBoard(t, db, cache, order, LeaderboardOperatorSet)
			owners := make([]string, 7)
			for i := range owners {
				owners[i] = uuid.Must(uuid.NewV4()).String()
				_, err := LeaderboardRecordWrite(ctx, logger, db, cache, ranks, uuid.Nil, b.Id, owners[i], "", int64(i/2), int64(i%2), `{}`, api.Operator_NO_OVERRIDE)
				require.NoError(t, err)
			}
			list := func(cursor string, limit int32) *api.LeaderboardRecordList {
				r, err := LeaderboardRecordsList(ctx, logger, db, cache, ranks, b.Id, wrapperspb.Int32(limit), cursor, owners, 0)
				require.NoError(t, err)
				return r
			}
			all := list("", 20)
			require.Len(t, all.Records, 7)
			require.Equal(t, int64(7), all.RankCount)
			require.Len(t, all.OwnerRecords, 7)
			first := list("", 2)
			second := list(first.NextCursor, 2)
			back := list(second.PrevCursor, 2)
			require.Equal(t, first.Records, back.Records)
			collected := append(first.Records, second.Records...)
			cursor := second.NextCursor
			for cursor != "" {
				p := list(cursor, 2)
				collected = append(collected, p.Records...)
				cursor = p.NextCursor
			}
			require.Equal(t, all.Records, collected)
			for _, position := range []int{0, 3, 6} {
				owner := uuid.Must(uuid.FromString(all.Records[position].OwnerId))
				around, err := LeaderboardRecordsHaystack(ctx, logger, db, cache, ranks, b.Id, "", owner, 3, 0)
				require.NoError(t, err)
				require.Len(t, around.Records, 3)
				found := false
				for _, r := range around.Records {
					if r.OwnerId == owner.String() {
						found = true
					}
				}
				require.True(t, found)
				if around.NextCursor != "" {
					_, err = LeaderboardRecordsHaystack(ctx, logger, db, cache, ranks, b.Id, around.NextCursor, owner, 3, 0)
					require.NoError(t, err)
				}
			}
			_, err := LeaderboardRecordsList(ctx, zap.NewNop(), db, cache, ranks, b.Id, wrapperspb.Int32(2), "invalid", nil, 0)
			require.ErrorIs(t, err, ErrLeaderboardInvalidCursor)
			_, err = LeaderboardRecordsList(ctx, zap.NewNop(), db, cache, ranks, b.Id, wrapperspb.Int32(2), first.NextCursor, nil, time.Now().Unix()+100)
			require.ErrorIs(t, err, ErrLeaderboardInvalidCursor)
			exported, err := LeaderboardRecordReadAll(ctx, logger, db, uuid.Must(uuid.FromString(owners[0])))
			require.NoError(t, err)
			require.Len(t, exported, 1)
			require.NoError(t, disableLeaderboardRanks(ctx, logger, db, cache, ranks, b.Id))
			require.False(t, cache.Get(b.Id).EnableRanks)
			deleted, err := cache.Delete(ctx, ranks, rankTestScheduler{}, b.Id)
			require.NoError(t, err)
			require.True(t, deleted)
			require.Nil(t, cache.Get(b.Id))
			require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM leaderboard_record WHERE leaderboard_id = ?", b.Id))
		})
	}
}

func TestDualBackendLeaderboardBoundsAndConcurrentWrites(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx := context.Background()
	b := rankTestBoard(t, db, cache, 1, LeaderboardOperatorIncrement)
	owner := uuid.Must(uuid.NewV4())
	gate := make(chan struct{})
	errs := make(chan error, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			_, err := LeaderboardRecordWrite(ctx, zap.NewNop(), db, cache, ranks, uuid.Nil, b.Id, owner.String(), "", 1, 0, "{}", api.Operator_NO_OVERRIDE)
			errs <- err
		}()
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	p, err := LeaderboardRecordsList(ctx, logger, db, cache, ranks, b.Id, nil, "", []string{owner.String()}, 0)
	require.NoError(t, err)
	require.Equal(t, int64(20), p.OwnerRecords[0].Score)
	require.Equal(t, int32(20), p.OwnerRecords[0].NumScore)
	_, err = LeaderboardRecordWrite(ctx, logger, db, cache, ranks, uuid.Nil, b.Id, owner.String(), "", math.MaxInt64, 0, "{}", api.Operator_SET)
	require.NoError(t, err)
	_, err = LeaderboardRecordWrite(ctx, zap.NewNop(), db, cache, ranks, uuid.Nil, b.Id, owner.String(), "", 1, 0, "{}", api.Operator_INCREMENT)
	require.Error(t, err)
	_, err = LeaderboardRecordWrite(ctx, zap.NewNop(), db, cache, ranks, uuid.Nil, b.Id, owner.String(), "", -1, 0, "{}", api.Operator_SET)
	require.Error(t, err)
	_, err = LeaderboardRecordWrite(ctx, zap.NewNop(), db, cache, ranks, uuid.Nil, b.Id, "bad uuid", "", 1, 0, "{}", api.Operator_SET)
	require.Error(t, err)
}

func TestDualBackendLeaderboardConsoleHandlers(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx := context.Background()
	board := rankTestBoard(t, db, cache, 1, LeaderboardOperatorSet)
	tour := rankTestTournament(t, db, cache, false, 3, 2)
	owner := uuid.Must(uuid.NewV4())
	server := &ConsoleServer{db: db, logger: zap.NewNop(), leaderboardCache: cache, leaderboardRankCache: ranks, leaderboardScheduler: rankTestScheduler{}}
	for _, b := range []*Leaderboard{board, tour} {
		var err error
		if b.IsTournament() {
			_, err = TournamentRecordWrite(ctx, logger, db, cache, ranks, uuid.Nil, b.Id, owner, "", 10, 0, `{}`, api.Operator_SET)
		} else {
			_, err = LeaderboardRecordWrite(ctx, logger, db, cache, ranks, uuid.Nil, b.Id, owner.String(), "", 10, 0, `{}`, api.Operator_SET)
		}
		require.NoError(t, err)
		definition, err := server.GetLeaderboard(ctx, &console.LeaderboardRequest{Id: b.Id})
		require.NoError(t, err)
		require.Equal(t, b.IsTournament(), definition.Tournament)
		page, err := server.ListLeaderboardRecords(ctx, &api.ListLeaderboardRecordsRequest{LeaderboardId: b.Id, Limit: wrapperspb.Int32(10)})
		require.NoError(t, err)
		require.Len(t, page.Records, 1)
		_, err = server.DeleteLeaderboardRecord(ctx, &console.DeleteLeaderboardRecordRequest{Id: b.Id, OwnerId: owner.String()})
		require.NoError(t, err)
		require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM leaderboard_record WHERE leaderboard_id = ?", b.Id))
	}
	list, err := server.ListLeaderboards(ctx, &console.LeaderboardListRequest{})
	require.NoError(t, err)
	require.Len(t, list.Leaderboards, 2)
	_, err = server.DeleteLeaderboard(ctx, &console.LeaderboardRequest{Id: board.Id})
	require.NoError(t, err)
	require.Nil(t, cache.Get(board.Id))
}
