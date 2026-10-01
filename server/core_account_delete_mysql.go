// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"go.uber.org/zap"
)

func deleteAccountMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, config Config, rankCache LeaderboardRankCache, sessions SessionRegistry, sessionCache SessionCache, tracker Tracker, storageIndex StorageIndex, userID uuid.UUID, recorded bool) error {
	var deleted bool
	var ranks []LeaderboardWithExpiry
	var objects StorageOpDeletes
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		// A deadlock retry must never retain results from a rolled-back attempt.
		deleted, ranks, objects = false, nil, nil
		var id string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", userID).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		// All native identity/storage mutations lock this owner first. Future
		// friendship/group/ranking writers must observe the same ownership fence.
		rows, err := tx.QueryContext(ctx, "SELECT source_id FROM user_edge WHERE destination_id = ? ORDER BY source_id FOR UPDATE", userID)
		if err != nil {
			return err
		}
		peers := []string{}
		for rows.Next() {
			var peer string
			if err := rows.Scan(&peer); err != nil {
				rows.Close()
				return err
			}
			if peer != id {
				peers = append(peers, peer)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, peer := range peers {
			if _, err := tx.ExecContext(ctx, "UPDATE users SET edge_count = edge_count - 1, update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", peer); err != nil {
				return err
			}
		}
		if err := deleteAccountGroupsMySQL(ctx, tx, userID); err != nil {
			return err
		}

		boards, err := lockAccountLeaderboardsMySQL(ctx, tx, userID)
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, "SELECT leaderboard_id, expiry_time FROM leaderboard_record WHERE owner_id = ? FOR UPDATE", userID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var leaderboard string
			var expiry time.Time
			if err := rows.Scan(&leaderboard, &expiry); err != nil {
				rows.Close()
				return err
			}
			ranks = append(ranks, LeaderboardWithExpiry{LeaderboardId: leaderboard, Expiry: expiry.Unix()})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM leaderboard_record WHERE owner_id = ?", userID); err != nil {
			return err
		}

		for _, board := range boards {
			if board.IsTournament() && board.HasMaxSize() {
				_, _, expiry := calculateTournamentDeadlines(board.StartTime, board.EndTime, int64(board.Duration), board.ResetSchedule, time.Now().UTC())
				if _, err := tournamentSizeMySQL(ctx, tx, board, expiry); err != nil {
					return err
				}
			}
		}

		rows, err = tx.QueryContext(ctx, "SELECT collection, `key` FROM storage WHERE user_id = ?", userID)
		if err != nil {
			return err
		}
		for rows.Next() {
			object := &api.DeleteStorageObjectId{}
			if err := rows.Scan(&object.Collection, &object.Key); err != nil {
				rows.Close()
				return err
			}
			objects = append(objects, &StorageOpDelete{OwnerID: id, ObjectID: object})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Foreign keys cascade devices, friends, storage, notes, messages,
		// notifications and wallet ledger. The schema trigger retains purchases
		// and subscriptions under the system owner in this same transaction.
		if _, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userID); err != nil {
			return err
		}
		if recorded {
			if _, err := tx.ExecContext(ctx, "INSERT INTO user_tombstone (user_id) VALUES (?) ON DUPLICATE KEY UPDATE user_id = user_id", userID); err != nil {
				return err
			}
		}
		deleted = true
		return nil
	})
	if err != nil {
		logger.Error("Error deleting MySQL account.", zap.Error(err), zap.String("user_id", userID.String()))
		return err
	}
	if !deleted {
		return nil
	}
	// No in-memory mutation occurs until all database work commits successfully.
	now := time.Now().UTC().Unix()
	for _, rank := range ranks {
		if rank.Expiry == 0 || rank.Expiry > now {
			rankCache.Delete(rank.LeaderboardId, rank.Expiry, userID)
		}
	}
	if storageIndex != nil {
		storageIndex.Delete(ctx, objects)
	}
	if err := SessionLogout(config, sessionCache, userID, "", ""); err != nil {
		return err
	}
	var disconnectErr error
	for _, presence := range tracker.ListPresenceIDByStream(PresenceStream{Mode: StreamModeNotifications, Subject: userID}) {
		// Attempt every session even if one disconnect fails after commit.
		disconnectErr = errors.Join(disconnectErr, sessions.Disconnect(ctx, presence.SessionID, false))
	}
	return disconnectErr
}

// Deleting the last superadmin deletes the group (upstream contract). Otherwise
// remove both directions, including superadmin edges, and decrement members only.
func deleteAccountGroupsMySQL(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	rows, err := tx.QueryContext(ctx, "SELECT g.id FROM `groups` g JOIN group_edge e ON e.source_id = g.id WHERE e.destination_id = ? ORDER BY g.id", userID)
	if err != nil {
		return err
	}
	groups := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, group := range groups {
		var id string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM `groups` WHERE id = ? FOR UPDATE", group).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		var state int
		if err := tx.QueryRowContext(ctx, "SELECT state FROM group_edge WHERE source_id = ? AND destination_id = ? FOR UPDATE", group, userID).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		deleteGroup := false
		if state == 0 {
			var others int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? AND destination_id <> ? AND state = 0", group, userID).Scan(&others); err != nil {
				return err
			}
			deleteGroup = others == 0
		}
		if deleteGroup {
			if _, err := tx.ExecContext(ctx, "DELETE FROM group_edge WHERE source_id = ? OR destination_id = ?", group, group); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM `groups` WHERE id = ?", group); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, "DELETE FROM group_edge WHERE (source_id = ? AND destination_id = ?) OR (source_id = ? AND destination_id = ?)", group, userID, userID, group); err != nil {
				return err
			}
			if state < 3 {
				if _, err := tx.ExecContext(ctx, "UPDATE `groups` SET edge_count = edge_count - 1, update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", group); err != nil {
					return err
				}
			}
		}
	}
	// group_edge intentionally has no foreign keys; also remove orphaned edges.
	_, err = tx.ExecContext(ctx, "DELETE FROM group_edge WHERE source_id = ? OR destination_id = ?", userID, userID)
	return err
}
