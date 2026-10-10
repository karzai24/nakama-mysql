// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/internal/cronexpr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Real scheduler loop and workers, with virtual time and no database I/O. Hooks
// add more boards while scheduling is active, exercising Update's reentrancy.
func TestLeaderboardScheduler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db, err := sql.Open("mysql", "") // Driver identity only; never connects.
		require.NoError(t, err)
		defer db.Close()
		cache := &LocalLeaderboardCache{leaderboards: map[string]*Leaderboard{}}
		ranks := &LocalLeaderboardRankCache{cache: map[LeaderboardWithExpiry]*RankCache{}}
		schedule := cronexpr.MustParse("* * * * *")
		add := func(id string) {
			cache.Lock()
			defer cache.Unlock()
			b := &Leaderboard{Id: id, ResetSchedule: schedule, Metadata: "{}"}
			cache.leaderboards[id] = b
			cache.allList = append(cache.allList, b)
		}
		for i := 0; i < 3; i++ {
			add(fmt.Sprintf("original-%d", i))
		}
		config := NewConfig(zap.NewNop())
		config.Leaderboard.CallbackQueueWorkers = 1
		scheduler := NewLocalLeaderboardScheduler(zap.NewNop(), db, config, cache, ranks).(*LocalLeaderboardScheduler)
		defer scheduler.Stop()
		events := make(chan string, 100)
		spawned := map[string]bool{}
		rt := &Runtime{leaderboardResetFunction: func(_ context.Context, b *api.Leaderboard, _ int64) error {
			if len(b.Id) >= 9 && b.Id[:9] == "original-" && !spawned[b.Id] {
				spawned[b.Id] = true
				add("spawned-" + b.Id)
				scheduler.Update()
			}
			events <- b.Id
			return nil
		}}
		scheduler.Start(rt)
		synctest.Wait()
		time.Sleep(61 * time.Second)
		synctest.Wait()
		require.Len(t, events, 3, "one hook per original board at the first reset")
		scheduler.Pause()
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Len(t, events, 3, "pause must suppress callbacks")
		scheduler.Resume()
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Len(t, events, 9, "resume schedules original and newly added boards")
		for i := 0; i < 100; i++ {
			scheduler.Update()
		}
		synctest.Wait()
		require.Len(t, events, 9, "updates must not duplicate an already fired reset")
		scheduler.Stop()
		synctest.Wait()
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		require.Len(t, events, 9, "stop must terminate timer and callback workers")
	})
}

// Drive a fixed completed period through the actual queue and database adapter.
// This replaces the inherited skipped test without deleting unrelated boards or
// waiting for a wall-clock minute. Timer behavior is covered separately above.
func TestDualBackendSchedulerDispatch(t *testing.T) {
	db, cache, ranks := rankTestCaches(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute).Add(-3 * time.Minute)
	fire := now.Add(time.Minute)
	var boards, tournaments []string
	for i := 0; i < 3; i++ {
		id := "scheduler_board_" + GenerateString()
		_, _, err := cache.Create(ctx, id, false, 1, 1, "* * * * *", "{}", true)
		require.NoError(t, err)
		boards = append(boards, id)
		boardID := id
		t.Cleanup(func() { lifecycleExec(t, db, "DELETE FROM leaderboard WHERE id = ?", boardID) })
		id = "scheduler_tournament_" + GenerateString()
		_, _, err = cache.CreateTournament(ctx, id, false, 1, 1, "* * * * *", "{}", "scheduler", "", 0, int(now.Add(-time.Hour).Unix()), 0, 60, 10, 0, false, true)
		require.NoError(t, err)
		tournaments = append(tournaments, id)
		tournamentID := id
		t.Cleanup(func() {
			lifecycleExec(t, db, "DELETE FROM leaderboard_record WHERE leaderboard_id = ?", tournamentID)
			lifecycleExec(t, db, "DELETE FROM leaderboard WHERE id = ?", tournamentID)
		})
		for j := 0; j < 2; j++ {
			lifecycleExec(t, db, "INSERT INTO leaderboard_record (leaderboard_id, owner_id, expiry_time, score) VALUES (?, ?, ?, ?)", id, uuid.Must(uuid.NewV4()), fire, j)
		}
		lifecycleExec(t, db, "UPDATE leaderboard SET size = 2 WHERE id = ?", id)
	}
	scheduler := NewLocalLeaderboardScheduler(zap.NewNop(), db, cfg, cache, ranks).(*LocalLeaderboardScheduler)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); scheduler.invokeCallback() }()
	t.Cleanup(func() {
		scheduler.Stop()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Error("scheduler worker did not stop")
		}
	})
	type event struct {
		kind, id string
		size     uint32
	}
	events := make(chan event, 9)
	scheduler.fnLeaderboardReset = func(_ context.Context, b *api.Leaderboard, reset int64) error {
		events <- event{"board", b.Id, 0}
		return nil
	}
	scheduler.fnTournamentEnd = func(_ context.Context, b *api.Tournament, _, _ int64) error {
		events <- event{"end", b.Id, b.Size}
		return nil
	}
	scheduler.fnTournamentReset = func(_ context.Context, b *api.Tournament, _, _ int64) error {
		events <- event{"reset", b.Id, b.Size}
		return nil
	}
	// Publish callback pointers before starting the worker's first invocation.
	// The queue send in processHooks synchronizes those reads.
	end, expiry, endIDs, expiryIDs := scheduler.computeNext(now)
	require.Equal(t, fire.Unix(), end)
	require.Equal(t, fire.Unix(), expiry)
	require.ElementsMatch(t, tournaments, endIDs)
	require.ElementsMatch(t, append(append([]string{}, boards...), tournaments...), expiryIDs)
	scheduler.Pause()
	scheduler.processHooks(fire, end, expiry, endIDs, expiryIDs)
	require.Empty(t, events)
	require.Empty(t, scheduler.queue)
	scheduler.Resume()
	scheduler.processHooks(fire, end, expiry, endIDs, expiryIDs)
	seen := map[string]bool{}
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for i := 0; i < 9; i++ {
		select {
		case e := <-events:
			key := e.kind + "/" + e.id
			require.False(t, seen[key], "duplicate callback %s", key)
			seen[key] = true
			if e.kind == "reset" {
				require.Equal(t, uint32(2), e.size)
			}
		case <-timeout.C:
			t.Fatal("scheduler callbacks did not finish")
		}
	}
	for _, id := range tournaments {
		require.True(t, seen["end/"+id])
		require.True(t, seen["reset/"+id])
		require.Zero(t, lifecycleCount(t, db, "SELECT size FROM leaderboard WHERE id = ?", id), "reset persisted before callback delivery")
	}
	for _, id := range boards {
		require.True(t, seen["board/"+id])
	}
}
