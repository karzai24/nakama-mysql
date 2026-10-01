// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const leaderboardRecordColumns = "leaderboard_id,owner_id,username,score,subscore,num_score,max_num_score,metadata,create_time,update_time,expiry_time"
const leaderboardPageColumnsMySQL = "owner_id,username,score,subscore,num_score,max_num_score,metadata,create_time,update_time"

func leaderboardPageMySQL(id string, expiry int64, limit int, ascending bool, cursor *leaderboardRecordListCursor) (string, []any, error) {
	query := "SELECT " + leaderboardPageColumnsMySQL + " FROM leaderboard_record WHERE leaderboard_id = ? AND expiry_time = ?"
	args := []any{id, time.Unix(expiry, 0).UTC()}
	if cursor != nil {
		owner, err := uuid.FromString(cursor.OwnerId)
		if err != nil {
			return "", nil, ErrLeaderboardInvalidCursor
		}
		if !cursor.IsNext {
			ascending = !ascending
		}
		op := "<"
		if ascending {
			op = ">"
		}
		query += " AND (score,subscore,owner_id) " + op + " (?,?,?)"
		args = append(args, cursor.Score, cursor.Subscore, owner)
	}
	if ascending {
		query += " ORDER BY score ASC,subscore ASC,owner_id ASC"
	} else {
		query += " ORDER BY score DESC,subscore DESC,owner_id DESC"
	}
	query += " LIMIT ?"
	args = append(args, limit+1)
	return query, args, nil
}

func leaderboardOwnersMySQL(id string, expiry int64, owners []string) (string, []any, error) {
	query := "SELECT " + leaderboardPageColumnsMySQL + " FROM leaderboard_record WHERE leaderboard_id = ? AND expiry_time = ? AND owner_id IN (" + mysqlMarks(len(owners)) + ")"
	args := []any{id, time.Unix(expiry, 0).UTC()}
	for _, owner := range owners {
		id, err := uuid.FromString(owner)
		if err != nil {
			return "", nil, err
		}
		args = append(args, id)
	}
	return query, args, nil
}

func leaderboardNeighborsMySQL(id string, expiry time.Time, score, subscore int64, owner uuid.UUID, limit int, ascending bool) (string, []any) {
	op, order := "<", "DESC"
	if ascending {
		op, order = ">", "ASC"
	}
	return "SELECT " + leaderboardRecordColumns + " FROM leaderboard_record WHERE leaderboard_id = ? AND expiry_time = ? AND (score,subscore,owner_id) " + op + " (?,?,?) ORDER BY score " + order + ",subscore " + order + ",owner_id " + order + " LIMIT ?", []any{id, expiry, score, subscore, owner, limit}
}

func scanLeaderboardRecord(row Scannable) (*api.LeaderboardRecord, error) {
	r := &api.LeaderboardRecord{}
	var username sql.NullString
	var created, updated, expiry time.Time
	if err := row.Scan(&r.LeaderboardId, &r.OwnerId, &username, &r.Score, &r.Subscore, &r.NumScore, &r.MaxNumScore, &r.Metadata, &created, &updated, &expiry); err != nil {
		return nil, err
	}
	if username.Valid {
		r.Username = wrapperspb.String(username.String)
	}
	r.CreateTime = &timestamppb.Timestamp{Seconds: created.Unix()}
	r.UpdateTime = &timestamppb.Timestamp{Seconds: updated.Unix()}
	if expiry.Unix() != 0 {
		r.ExpiryTime = &timestamppb.Timestamp{Seconds: expiry.Unix()}
	}
	return r, nil
}

// Leaderboard owners may be virtual IDs (for example a team), not just users.
// Existing users share the account-deletion lock; the leaderboard row serializes
// virtual owners, score writers, capacity changes, and leaderboard deletion.
func lockLeaderboardMySQL(ctx context.Context, tx *sql.Tx, id string, owner uuid.UUID) (*Leaderboard, error) {
	var user string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", owner).Scan(&user); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	l, err := scanLeaderboardDefinition(tx.QueryRowContext(ctx, "SELECT "+leaderboardDefinitionColumns+" FROM leaderboard WHERE id = ? FOR UPDATE", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLeaderboardNotFound
	}
	return l, err
}

func leaderboardOperator(base int, override api.Operator) (int, error) {
	if override == api.Operator_NO_OVERRIDE {
		return base, nil
	}
	for op, value := range OperatorIntToEnum {
		if value == override {
			return op, nil
		}
	}
	return 0, ErrInvalidOperator
}

func leaderboardScore(old, value int64, operator, order int) (int64, error) {
	if value < 0 {
		return 0, errors.New("leaderboard score must not be negative")
	}
	switch operator {
	case LeaderboardOperatorIncrement:
		if old > math.MaxInt64-value {
			return 0, errors.New("leaderboard score exceeds int64 maximum")
		}
		return old + value, nil
	case LeaderboardOperatorDecrement:
		if value >= old {
			return 0, nil
		}
		return old - value, nil
	case LeaderboardOperatorSet:
		return value, nil
	default:
		if (order == LeaderboardSortOrderAscending && value < old) || (order == LeaderboardSortOrderDescending && value > old) {
			return value, nil
		}
		return old, nil
	}
}

func leaderboardRecordWriteMySQL(ctx context.Context, db *sql.DB, ranks LeaderboardRankCache, caller uuid.UUID, id string, owner uuid.UUID, username string, score, subscore int64, metadata string, override api.Operator, tournament bool) (*api.LeaderboardRecord, error) {
	var record *api.LeaderboardRecord
	var board *Leaderboard
	var expiry int64
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		record = nil
		var err error
		board, err = lockLeaderboardMySQL(ctx, tx, id, owner)
		if err != nil {
			if tournament && errors.Is(err, ErrLeaderboardNotFound) {
				return runtime.ErrTournamentNotFound
			}
			return err
		}
		if tournament && !board.IsTournament() {
			return runtime.ErrTournamentNotFound
		}
		if !tournament && board.IsTournament() {
			return ErrLeaderboardNotFound
		}
		if board.Authoritative && caller != uuid.Nil {
			if tournament {
				return runtime.ErrTournamentAuthoritative
			}
			return ErrLeaderboardAuthoritative
		}
		now := time.Now().UTC()
		expiry = 0
		if tournament {
			start, end, expires := calculateTournamentDeadlines(board.StartTime, board.EndTime, int64(board.Duration), board.ResetSchedule, now)
			if start > now.Unix() || end <= now.Unix() {
				return runtime.ErrTournamentOutsideDuration
			}
			expiry = expires
		} else if board.ResetSchedule != nil {
			expiry = board.ResetSchedule.Next(now).Unix()
		}
		operator, err := leaderboardOperator(board.Operator, override)
		if err != nil {
			return err
		}
		record, err = scanLeaderboardRecord(tx.QueryRowContext(ctx, "SELECT "+leaderboardRecordColumns+" FROM leaderboard_record WHERE leaderboard_id = ? AND owner_id = ? AND expiry_time = ? FOR UPDATE", id, owner, time.Unix(expiry, 0).UTC()))
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if tournament && !exists && board.JoinRequired {
			return runtime.ErrTournamentWriteJoinRequired
		}
		newScore, newSub := score, subscore
		changed := true
		if exists {
			if tournament && record.NumScore >= int32(record.MaxNumScore) {
				return runtime.ErrTournamentWriteMaxNumScoreReached
			}
			newScore, err = leaderboardScore(record.Score, score, operator, board.SortOrder)
			if err != nil {
				return err
			}
			newSub, err = leaderboardScore(record.Subscore, subscore, operator, board.SortOrder)
			if err != nil {
				return err
			}
			changed = tournament || newScore != record.Score || newSub != record.Subscore
			// Decrement attempts count even when clamped at zero, as upstream does.
			if operator == LeaderboardOperatorDecrement && (score != 0 || subscore != 0) {
				changed = true
			}
			if changed && record.NumScore == math.MaxInt32 {
				return errors.New("leaderboard submission count exceeds int32 maximum")
			}
		} else {
			if score < 0 || subscore < 0 {
				return errors.New("leaderboard score must not be negative")
			}
			if operator == LeaderboardOperatorDecrement {
				newScore, newSub = 0, 0
			}
		}
		if changed {
			if !exists {
				if tournament && board.HasMaxSize() {
					size, err := tournamentSizeMySQL(ctx, tx, board, expiry)
					if err != nil {
						return err
					}
					if size >= board.MaxSize {
						return runtime.ErrTournamentMaxSizeReached
					}
				}
				max := 1000000
				if tournament {
					max = board.MaxNumScore
				}
				insertMetadata := metadata
				if insertMetadata == "" {
					insertMetadata = "{}"
				}
				var name any
				if username != "" {
					name = username
				}
				_, err = tx.ExecContext(ctx, "INSERT INTO leaderboard_record (leaderboard_id,owner_id,username,score,subscore,metadata,expiry_time,max_num_score) VALUES (?,?,?,?,?,?,?,?)", id, owner, name, newScore, newSub, insertMetadata, time.Unix(expiry, 0).UTC(), max)
			} else {
				var name, meta any
				if username != "" {
					name = username
				}
				if metadata != "" {
					meta = metadata
				}
				_, err = tx.ExecContext(ctx, "UPDATE leaderboard_record SET score = ?,subscore = ?,num_score = num_score + 1,username = COALESCE(?,username),metadata = COALESCE(CAST(? AS JSON),metadata),update_time = CURRENT_TIMESTAMP(6) WHERE leaderboard_id = ? AND owner_id = ? AND expiry_time = ?", newScore, newSub, name, meta, id, owner, time.Unix(expiry, 0).UTC())
			}
			if err != nil {
				return err
			}
			if tournament && board.HasMaxSize() {
				if _, err = tournamentSizeMySQL(ctx, tx, board, expiry); err != nil {
					return err
				}
			}
		}
		record, err = scanLeaderboardRecord(tx.QueryRowContext(ctx, "SELECT "+leaderboardRecordColumns+" FROM leaderboard_record WHERE leaderboard_id = ? AND owner_id = ? AND expiry_time = ?", id, owner, time.Unix(expiry, 0).UTC()))
		return err
	})
	if err != nil {
		return nil, err
	}
	record.Rank = ranks.Insert(id, board.SortOrder, record.Score, record.Subscore, record.NumScore, expiry, owner, board.EnableRanks)
	return record, nil
}

// Called under the leaderboard row lock. Recounting the selected period prevents
// delayed reset callbacks or account deletion from corrupting capacity accounting.
func tournamentSizeMySQL(ctx context.Context, tx *sql.Tx, board *Leaderboard, expiry int64) (int, error) {
	var size int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM leaderboard_record WHERE leaderboard_id = ? AND expiry_time = ?", board.Id, time.Unix(expiry, 0).UTC()).Scan(&size); err != nil {
		return 0, err
	}
	_, err := tx.ExecContext(ctx, "UPDATE leaderboard SET size = ? WHERE id = ?", size, board.Id)
	return size, err
}

func leaderboardRecordDeleteMySQL(ctx context.Context, db *sql.DB, ranks LeaderboardRankCache, caller uuid.UUID, id, ownerID string, tournament bool) error {
	owner, err := uuid.FromString(ownerID)
	if err != nil {
		return err
	}
	var expiry int64
	err = executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		board, err := lockLeaderboardMySQL(ctx, tx, id, owner)
		if err != nil {
			if tournament && errors.Is(err, ErrLeaderboardNotFound) {
				return runtime.ErrTournamentNotFound
			}
			return err
		}
		if board.IsTournament() != tournament {
			if tournament {
				return runtime.ErrTournamentNotFound
			}
			return ErrLeaderboardNotFound
		}
		if board.Authoritative && caller != uuid.Nil {
			if tournament {
				return runtime.ErrTournamentAuthoritative
			}
			return ErrLeaderboardAuthoritative
		}
		now := time.Now().UTC()
		expiry = 0
		if tournament {
			_, _, expiry = calculateTournamentDeadlines(board.StartTime, board.EndTime, int64(board.Duration), board.ResetSchedule, now)
		} else if board.ResetSchedule != nil {
			expiry = board.ResetSchedule.Next(now).Unix()
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM leaderboard_record WHERE leaderboard_id = ? AND owner_id = ? AND expiry_time = ?", id, owner, time.Unix(expiry, 0).UTC()); err != nil {
			return err
		}
		if tournament && board.HasMaxSize() {
			_, err = tournamentSizeMySQL(ctx, tx, board, expiry)
		}
		return err
	})
	if err == nil {
		ranks.Delete(id, expiry, owner)
	}
	return err
}
