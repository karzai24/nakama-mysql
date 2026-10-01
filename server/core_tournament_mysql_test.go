// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
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

func TestDualBackendTournamentLifecycle(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx := context.Background()
	for _, join := range []bool{false, true} {
		t.Run(fmt.Sprint(join), func(t *testing.T) {
			b := rankTestTournament(t, db, cache, join, 2, 2)
			a, bOwner, c := uuid.Must(uuid.NewV4()), uuid.Must(uuid.NewV4()), uuid.Must(uuid.NewV4())
			write := func(owner uuid.UUID, score int64) (*api.LeaderboardRecord, error) {
				return TournamentRecordWrite(ctx, zap.NewNop(), db, cache, ranks, uuid.Nil, b.Id, owner, "player", score, 0, `{"value":1}`, api.Operator_NO_OVERRIDE)
			}
			if join {
				_, err := write(a, 10)
				require.ErrorIs(t, err, runtime.ErrTournamentWriteJoinRequired)
				require.NoError(t, TournamentJoin(ctx, logger, db, cache, ranks, a, "player", b.Id))
				require.NoError(t, TournamentJoin(ctx, logger, db, cache, ranks, a, "player", b.Id))
			}
			r, err := write(a, 10)
			require.NoError(t, err)
			require.Equal(t, int32(1), r.NumScore)
			r, err = write(a, 20)
			require.NoError(t, err)
			require.Equal(t, int32(2), r.NumScore)
			r, err = write(a, 30)
			require.ErrorIs(t, err, runtime.ErrTournamentWriteMaxNumScoreReached)
			require.NoError(t, TournamentAddAttempt(ctx, logger, db, cache, b.Id, a.String(), 1))
			r, err = write(a, 30)
			require.NoError(t, err)
			require.Equal(t, int32(3), r.NumScore)
			require.Equal(t, uint32(3), r.MaxNumScore)
			if join {
				require.NoError(t, TournamentJoin(ctx, logger, db, cache, ranks, bOwner, "second", b.Id))
			}
			_, err = write(bOwner, 40)
			require.NoError(t, err)
			if join {
				err = TournamentJoin(ctx, zap.NewNop(), db, cache, ranks, c, "third", b.Id)
			} else {
				_, err = write(c, 50)
			}
			require.ErrorIs(t, err, runtime.ErrTournamentMaxSizeReached)
			fetched, err := TournamentsGet(ctx, logger, db, cache, []string{b.Id, b.Id, "missing"})
			require.NoError(t, err)
			require.Len(t, fetched, 1)
			require.Equal(t, uint32(2), fetched[0].Size)
			require.False(t, fetched[0].CanEnter)
			listed, err := TournamentList(ctx, logger, db, cache, 0, 127, 0, -1, 100, nil)
			require.NoError(t, err)
			require.NotEmpty(t, listed.Tournaments)
			records, err := TournamentRecordsList(ctx, logger, db, cache, ranks, b.Id, []string{a.String()}, wrapperspb.Int32(10), "", 0)
			require.NoError(t, err)
			require.Len(t, records.Records, 2)
			require.Len(t, records.OwnerRecords, 1)
			around, err := TournamentRecordsHaystack(ctx, logger, db, cache, ranks, b.Id, "", a, 2, 0)
			require.NoError(t, err)
			require.Len(t, around.Records, 2)
			require.NoError(t, TournamentRecordDelete(ctx, logger, db, cache, ranks, uuid.Nil, b.Id, bOwner.String()))
			require.Equal(t, 1, lifecycleCount(t, db, "SELECT size FROM leaderboard WHERE id = ?", b.Id))
			if join {
				require.NoError(t, TournamentJoin(ctx, logger, db, cache, ranks, c, "third", b.Id))
			}
			_, err = write(c, 50)
			require.NoError(t, err)
			require.NoError(t, DisableTournamentRanks(ctx, logger, db, cache, ranks, b.Id))
			require.True(t, cache.Get(b.Id).IsTournament())
			require.False(t, cache.Get(b.Id).EnableRanks)
			r, err = write(c, 51)
			require.NoError(t, err)
			require.Zero(t, r.Rank)
			require.NoError(t, TournamentDelete(ctx, cache, ranks, rankTestScheduler{}, b.Id))
			require.Nil(t, cache.Get(b.Id))
		})
	}
}

func TestDualBackendRankDefinitionsAndAuthorization(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx := context.Background()
	b := rankTestBoard(t, db, cache, 1, LeaderboardOperatorSet)
	existing, created, err := cache.Create(ctx, b.Id, true, 0, 0, "", `{}`, false)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, b, existing)
	_, _, err = cache.CreateTournament(ctx, b.Id, false, 1, 0, "", "", "", "", 0, 0, 0, 60, 0, 0, false, true)
	require.Error(t, err)
	id := "authoritative_" + GenerateString()
	auth, _, err := cache.Create(ctx, id, true, 1, 0, "", `{}`, true)
	require.NoError(t, err)
	t.Cleanup(func() { lifecycleExec(t, db, "DELETE FROM leaderboard WHERE id = ?", id) })
	owner := uuid.Must(uuid.NewV4())
	_, err = LeaderboardRecordWrite(ctx, logger, db, cache, ranks, owner, auth.Id, owner.String(), "", 5, 0, `{}`, api.Operator_NO_OVERRIDE)
	require.ErrorIs(t, err, ErrLeaderboardAuthoritative)
	require.ErrorIs(t, LeaderboardRecordDelete(ctx, logger, db, cache, ranks, owner, auth.Id, owner.String()), ErrLeaderboardAuthoritative)
	tour := rankTestTournament(t, db, cache, false, 0, 0)
	lifecycleExec(t, db, "UPDATE leaderboard SET authoritative = ? WHERE id = ?", true, tour.Id)
	require.NoError(t, cache.RefreshAllLeaderboards(ctx))
	_, err = TournamentRecordWrite(ctx, logger, db, cache, ranks, owner, tour.Id, owner, "", 5, 0, `{}`, api.Operator_NO_OVERRIDE)
	require.ErrorIs(t, err, runtime.ErrTournamentAuthoritative)
	require.Equal(t, tour.Duration, cache.Get(tour.Id).Duration)
	// Separate cache creates against an existing database row must read the winner.
	other := &LocalLeaderboardCache{db: db, logger: zap.NewNop(), leaderboards: map[string]*Leaderboard{}}
	stored, _, err := other.CreateTournament(ctx, tour.Id, false, 0, 0, "", `{"different":true}`, "different", "", 4, 0, 0, 20, 5, 2, true, false)
	require.NoError(t, err)
	require.Equal(t, tour.Duration, stored.Duration)
	require.Equal(t, tour.Metadata, stored.Metadata)
	require.True(t, stored.Authoritative)
	require.True(t, stored.EnableRanks)
	// Invalid metadata must never populate the cache.
	bad := "bad_" + GenerateString()
	_, _, err = cache.Create(ctx, bad, false, 1, 0, "", "invalid JSON", true)
	require.Error(t, err)
	require.Nil(t, cache.Get(bad))
}

func TestDualBackendTournamentRacesRollbackAndDeletion(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx := context.Background()
	for _, join := range []bool{false, true} {
		t.Run(fmt.Sprint(join), func(t *testing.T) {
			b := rankTestTournament(t, db, cache, join, 2, 2)
			start := make(chan struct{})
			outcomes := make(chan error, 12)
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					owner := uuid.Must(uuid.NewV4())
					var err error
					if join {
						err = TournamentJoin(ctx, zap.NewNop(), db, cache, ranks, owner, "", b.Id)
					} else {
						_, err = TournamentRecordWrite(ctx, zap.NewNop(), db, cache, ranks, uuid.Nil, b.Id, owner, "", 5, 0, `{}`, api.Operator_NO_OVERRIDE)
					}
					outcomes <- err
				}()
			}
			close(start)
			wg.Wait()
			close(outcomes)
			success := 0
			for err := range outcomes {
				if err == nil {
					success++
				} else {
					require.ErrorIs(t, err, runtime.ErrTournamentMaxSizeReached)
				}
			}
			require.Equal(t, 2, success)
			require.Equal(t, 2, lifecycleCount(t, db, "SELECT size FROM leaderboard WHERE id = ?", b.Id))
			require.Equal(t, 2, lifecycleCount(t, db, "SELECT COUNT(*) FROM leaderboard_record WHERE leaderboard_id = ?", b.Id))
		})
	}
	b := rankTestTournament(t, db, cache, false, 2, 2)
	user, _ := lifecycleUser(t, db)
	drop := failureTrigger(t, db, "leaderboard", "UPDATE", "NEW.id = '"+b.Id+"' AND NEW.size = 1")
	_, err := TournamentRecordWrite(ctx, zap.NewNop(), db, cache, ranks, uuid.Nil, b.Id, user, "", 10, 0, `{}`, api.Operator_NO_OVERRIDE)
	require.Error(t, err)
	require.Zero(t, lifecycleCount(t, db, "SELECT size FROM leaderboard WHERE id = ?", b.Id))
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM leaderboard_record WHERE leaderboard_id = ?", b.Id))
	_, _, expiry := calculateTournamentDeadlines(b.StartTime, b.EndTime, int64(b.Duration), b.ResetSchedule, time.Now().UTC())
	require.Zero(t, ranks.Get(b.Id, expiry, user))
	drop()
	_, err = TournamentRecordWrite(ctx, logger, db, cache, ranks, uuid.Nil, b.Id, user, "", 10, 0, `{}`, api.Operator_NO_OVERRIDE)
	require.NoError(t, err)
	require.Error(t, TournamentAddAttempt(ctx, zap.NewNop(), db, cache, b.Id, user.String(), -2), "must propagate constraint failure")
	sessionCache := NewLocalSessionCache(60, 600)
	defer sessionCache.Stop()
	require.NoError(t, DeleteAccount(ctx, logger, db, nil, nil, ranks, &lifecycleSessions{}, sessionCache, lifecycleTracker{}, user, false, nil))
	require.Zero(t, lifecycleCount(t, db, "SELECT size FROM leaderboard WHERE id = ?", b.Id))
	require.Zero(t, ranks.Get(b.Id, expiry, user))
}

func TestDualBackendTournamentCallbacksAndPeriodIsolation(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now().UTC()
	id := "reset_" + GenerateString()
	b, _, err := cache.CreateTournament(ctx, id, false, 1, 1, "0 * * * *", `{}`, "reset", "", 0, int(now.Add(-3*time.Hour).Unix()), 0, 3600, 3, 2, false, true)
	require.NoError(t, err)
	t.Cleanup(func() { lifecycleExec(t, db, "DELETE FROM leaderboard WHERE id = ?", id) })
	owner := uuid.Must(uuid.NewV4())
	r, err := TournamentRecordWrite(ctx, logger, db, cache, ranks, uuid.Nil, id, owner, "", 10, 0, `{}`, api.Operator_NO_OVERRIDE)
	require.NoError(t, err)
	event := now.Truncate(time.Hour).Add(-time.Second)
	_, _, oldExpiry := calculateTournamentDeadlines(b.StartTime, b.EndTime, int64(b.Duration), b.ResetSchedule, event)
	require.NotEqual(t, oldExpiry, r.ExpiryTime.Seconds)
	for i := 0; i < 2; i++ {
		lifecycleExec(t, db, "INSERT INTO leaderboard_record (leaderboard_id,owner_id,expiry_time,score) VALUES (?,?,?,?)", id, uuid.Must(uuid.NewV4()), time.Unix(oldExpiry, 0).UTC(), 5+i)
	}
	rewardUser, _ := lifecycleUser(t, db)
	callbacks := make(chan *api.Tournament, 2)
	scheduler := NewLocalLeaderboardScheduler(zap.NewNop(), db, cfg, cache, ranks).(*LocalLeaderboardScheduler)
	defer scheduler.Stop()
	scheduler.fnTournamentReset = func(ctx context.Context, tour *api.Tournament, _, _ int64) error {
		n := &RuntimeGoNakamaModule{db: db, logger: zap.NewNop()}
		if _, _, err := n.WalletUpdate(ctx, rewardUser.String(), map[string]int64{"reward": 7}, nil, true); err != nil {
			return err
		}
		callbacks <- tour
		return nil
	}
	scheduler.fnTournamentEnd = func(_ context.Context, tour *api.Tournament, _, _ int64) error { callbacks <- tour; return nil }
	go scheduler.invokeCallback()
	scheduler.queue <- &LeaderboardSchedulerCallback{id: id, leaderboard: b, t: event}
	select {
	case got := <-callbacks:
		require.Equal(t, uint32(2), got.Size)
	case <-time.After(3 * time.Second):
		t.Fatal("reset callback did not run")
	}
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT size FROM leaderboard WHERE id = ?", id), "late reset must preserve new-period entrants")
	require.Equal(t, int64(7), walletBalance(t, db, rewardUser)["reward"], "runtime reward callback must execute after reset commit")
	scheduler.queue <- &LeaderboardSchedulerCallback{id: id, t: event}
	select {
	case got := <-callbacks:
		require.Equal(t, uint32(2), got.Size)
	case <-time.After(3 * time.Second):
		t.Fatal("end callback did not run")
	}
	page, err := TournamentRecordsList(ctx, logger, db, cache, ranks, id, nil, wrapperspb.Int32(10), "", oldExpiry)
	require.NoError(t, err)
	require.Len(t, page.Records, 2)
	page, err = TournamentRecordsList(ctx, logger, db, cache, ranks, id, nil, wrapperspb.Int32(10), "", 0)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	prepare := tournamentCallbackMySQL
	{
		prepare = tournamentCallbackMySQL
	}
	_, err = prepare(ctx, db, "missing", event, true)
	require.True(t, errors.Is(err, sql.ErrNoRows))
}

func TestDualBackendTournamentAttemptRaceAndHaystack(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx := context.Background()
	b := rankTestTournament(t, db, cache, true, 10, 2)
	owner := uuid.Must(uuid.NewV4())
	require.NoError(t, TournamentJoin(ctx, logger, db, cache, ranks, owner, "", b.Id))
	gate := make(chan struct{})
	outcomes := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			_, err := TournamentRecordWrite(ctx, zap.NewNop(), db, cache, ranks, uuid.Nil, b.Id, owner, "", 1, 0, `{}`, api.Operator_INCREMENT)
			outcomes <- err
		}()
	}
	close(gate)
	wg.Wait()
	close(outcomes)
	success := 0
	for err := range outcomes {
		if err == nil {
			success++
		} else {
			require.ErrorIs(t, err, runtime.ErrTournamentWriteMaxNumScoreReached)
		}
	}
	require.Equal(t, 2, success)
	_, err := LeaderboardRecordWrite(ctx, zap.NewNop(), db, cache, ranks, uuid.Nil, b.Id, owner.String(), "", 99, 0, `{}`, api.Operator_SET)
	require.ErrorIs(t, err, ErrLeaderboardNotFound, "ordinary leaderboard endpoint must not bypass tournament limits")
	var middle uuid.UUID
	for i := 0; i < 6; i++ {
		user := uuid.Must(uuid.NewV4())
		if i == 3 {
			middle = user
		}
		require.NoError(t, TournamentJoin(ctx, logger, db, cache, ranks, user, "", b.Id))
		_, err := TournamentRecordWrite(ctx, logger, db, cache, ranks, uuid.Nil, b.Id, user, "", int64(i+10), 0, `{}`, api.Operator_SET)
		require.NoError(t, err)
	}
	around, err := TournamentRecordsHaystack(ctx, logger, db, cache, ranks, b.Id, "", middle, 3, 0)
	require.NoError(t, err)
	require.NotEmpty(t, around.PrevCursor)
	require.NotEmpty(t, around.NextCursor)
	require.NotEqual(t, around.PrevCursor, around.NextCursor)
	previous, err := TournamentRecordsHaystack(ctx, logger, db, cache, ranks, b.Id, around.PrevCursor, middle, 3, 0)
	require.NoError(t, err)
	for _, record := range previous.Records {
		require.Greater(t, record.Score, around.Records[0].Score)
	}
}

func TestDualBackendLeaderboardConcurrentCreate(t *testing.T) {
	db, _, _ := rankTestCaches(t)
	id := "create_" + GenerateString()
	t.Cleanup(func() { lifecycleExec(t, db, "DELETE FROM leaderboard WHERE id = ?", id) })
	type outcome struct {
		board   *Leaderboard
		created bool
		err     error
	}
	results := make(chan outcome, 8)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cache := &LocalLeaderboardCache{db: db, logger: zap.NewNop(), leaderboards: map[string]*Leaderboard{}}
			b, created, err := cache.Create(context.Background(), id, false, i%2, LeaderboardOperatorSet, "", fmt.Sprintf(`{"writer":%d}`, i), i%2 == 0)
			results <- outcome{b, created, err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	count := 0
	var winner *Leaderboard
	for result := range results {
		require.NoError(t, result.err)
		if winner == nil {
			winner = result.board
		}
		require.Equal(t, winner, result.board)
		if result.created {
			count++
		}
	}
	require.Equal(t, 1, count)
}
