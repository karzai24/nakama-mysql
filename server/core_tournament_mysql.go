// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

func tournamentJoinMySQL(ctx context.Context, db *sql.DB, ranks LeaderboardRankCache, owner uuid.UUID, username, id string) error {
	var board *Leaderboard
	var expiry int64
	var joined bool
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		joined = false
		var err error
		board, err = lockLeaderboardMySQL(ctx, tx, id, owner)
		if errors.Is(err, ErrLeaderboardNotFound) {
			return runtime.ErrTournamentNotFound
		}
		if err != nil {
			return err
		}
		if !board.IsTournament() {
			return runtime.ErrTournamentNotFound
		}
		if !board.JoinRequired {
			return nil
		}
		now := time.Now().UTC()
		_, end, expires := calculateTournamentDeadlines(board.StartTime, board.EndTime, int64(board.Duration), board.ResetSchedule, now)
		expiry = expires
		if end <= now.Unix() {
			return runtime.ErrTournamentOutsideDuration
		}
		var exists int
		err = tx.QueryRowContext(ctx, "SELECT 1 FROM leaderboard_record WHERE leaderboard_id = ? AND owner_id = ? AND expiry_time = ?", id, owner, time.Unix(expiry, 0).UTC()).Scan(&exists)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if board.HasMaxSize() {
			size, err := tournamentSizeMySQL(ctx, tx, board, expiry)
			if err != nil {
				return err
			}
			if size >= board.MaxSize {
				return runtime.ErrTournamentMaxSizeReached
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO leaderboard_record (leaderboard_id,owner_id,expiry_time,username,num_score,max_num_score) VALUES (?,?,?,?,0,?)", id, owner, time.Unix(expiry, 0).UTC(), username, board.MaxNumScore); err != nil {
			return err
		}
		if board.HasMaxSize() {
			if _, err = tournamentSizeMySQL(ctx, tx, board, expiry); err != nil {
				return err
			}
		}
		joined = true
		return nil
	})
	if err == nil && joined {
		ranks.Insert(id, board.SortOrder, 0, 0, 0, expiry, owner, board.EnableRanks)
	}
	return err
}

func tournamentAddAttemptMySQL(ctx context.Context, db *sql.DB, id, ownerID string, count int) error {
	if count == 0 {
		return nil
	}
	owner, err := uuid.FromString(ownerID)
	if err != nil {
		return err
	}
	return executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		board, err := lockLeaderboardMySQL(ctx, tx, id, owner)
		if errors.Is(err, ErrLeaderboardNotFound) {
			return runtime.ErrTournamentNotFound
		}
		if err != nil {
			return err
		}
		if !board.IsTournament() {
			return runtime.ErrTournamentNotFound
		}
		now := time.Now().UTC()
		_, end, expiry := calculateTournamentDeadlines(board.StartTime, board.EndTime, int64(board.Duration), board.ResetSchedule, now)
		if end <= now.Unix() {
			return runtime.ErrTournamentOutsideDuration
		}
		_, err = tx.ExecContext(ctx, "UPDATE leaderboard_record SET max_num_score = max_num_score + ? WHERE leaderboard_id = ? AND owner_id = ? AND expiry_time = ?", count, id, owner, time.Unix(expiry, 0).UTC())
		return err
	})
}

// Use the event's period for hook data, and the current period for capacity. A
// delayed reset must not zero the count of players already in the new period.
func tournamentCallbackMySQL(ctx context.Context, db *sql.DB, id string, event time.Time, reset bool) (*api.Tournament, error) {
	var result *api.Tournament
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		board, err := scanLeaderboardDefinition(tx.QueryRowContext(ctx, "SELECT "+leaderboardDefinitionColumns+" FROM leaderboard WHERE id = ? FOR UPDATE", id))
		if err != nil {
			return err
		}
		result, err = parseTournament(tx.QueryRowContext(ctx, "SELECT "+tournamentDefinitionColumns+" FROM leaderboard WHERE id = ?", id), event)
		if err != nil {
			return err
		}
		result.Authoritative = board.Authoritative
		if board.HasMaxSize() {
			_, _, expiry := calculateTournamentDeadlines(board.StartTime, board.EndTime, int64(board.Duration), board.ResetSchedule, event)
			var size uint32
			if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM leaderboard_record WHERE leaderboard_id = ? AND expiry_time = ?", id, time.Unix(expiry, 0).UTC()).Scan(&size); err != nil {
				return err
			}
			result.Size = size
			result.CanEnter = int64(result.StartActive) <= event.Unix() && (result.EndActive == 0 || int64(result.EndActive) >= event.Unix()) && size < uint32(board.MaxSize)
			if reset {
				_, _, current := calculateTournamentDeadlines(board.StartTime, board.EndTime, int64(board.Duration), board.ResetSchedule, time.Now().UTC())
				_, err = tournamentSizeMySQL(ctx, tx, board, current)
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func tournamentQueryMySQL(columns string, ids []string) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return "SELECT " + columns + " FROM leaderboard WHERE id IN (" + mysqlMarks(len(ids)) + ")", args
}

// Account deletion already owns the user lock. Lock its boards before records so
// deleting scores can also repair the current tournament counts atomically.
func lockAccountLeaderboardsMySQL(ctx context.Context, tx *sql.Tx, owner uuid.UUID) ([]*Leaderboard, error) {
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT leaderboard_id FROM leaderboard_record WHERE owner_id = ? ORDER BY leaderboard_id", owner)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	boards := make([]*Leaderboard, 0, len(ids))
	for _, id := range ids {
		board, err := scanLeaderboardDefinition(tx.QueryRowContext(ctx, "SELECT "+leaderboardDefinitionColumns+" FROM leaderboard WHERE id = ? FOR UPDATE", id))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		boards = append(boards, board)
	}
	return boards, nil
}
