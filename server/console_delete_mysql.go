// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/emptypb"
)

// These destructive console maintenance operations are transactional on MySQL.
// Run them with application traffic quiesced; they are not an online reset protocol.
func (s *ConsoleServer) clearDataMySQL(ctx context.Context, storageOnly bool) (*emptypb.Empty, error) {
	var objects StorageOpDeletes
	var users, groups []uuid.UUID
	var boards []string
	var ranks []LeaderboardWithExpiry
	err := executeInTxMySQL(ctx, s.db, func(tx *sql.Tx) error {
		objects = nil
		users = nil
		groups = nil
		boards = nil
		ranks = nil
		// Follow the same user-before-storage locking order as ordinary writes.
		rows, err := tx.QueryContext(ctx, "SELECT id FROM users ORDER BY id FOR UPDATE")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uuid.UUID
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			users = append(users, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, "SELECT user_id,collection,`key` FROM storage")
		if err != nil {
			return err
		}
		for rows.Next() {
			o := &StorageOpDelete{ObjectID: &api.DeleteStorageObjectId{}}
			if err = rows.Scan(&o.OwnerID, &o.ObjectID.Collection, &o.ObjectID.Key); err != nil {
				rows.Close()
				return err
			}
			objects = append(objects, o)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if storageOnly {
			_, err = tx.ExecContext(ctx, "DELETE FROM storage")
			return err
		}
		rows, err = tx.QueryContext(ctx, "SELECT id FROM leaderboard ORDER BY id FOR UPDATE")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			boards = append(boards, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, "SELECT DISTINCT leaderboard_id,expiry_time FROM leaderboard_record")
		if err != nil {
			return err
		}
		for rows.Next() {
			var r LeaderboardWithExpiry
			var ts time.Time
			if err = rows.Scan(&r.LeaderboardId, &ts); err != nil {
				rows.Close()
				return err
			}
			r.Expiry = ts.Unix()
			ranks = append(ranks, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, "SELECT id FROM `groups` FOR UPDATE")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uuid.UUID
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			groups = append(groups, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		// Identifiers below are a fixed internal list, never request input.
		for _, table := range []string{"user_edge", "user_device", "user_tombstone", "wallet_ledger", "storage", "purchase", "subscription", "notification", "message", "leaderboard_record", "leaderboard", "group_edge", "groups", "users_notes", "users"} {
			if _, err = tx.ExecContext(ctx, "DELETE FROM `"+table+"`"); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO users (id,username) VALUES (?, '')", uuid.Nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	if s.storageIndex != nil {
		s.storageIndex.Delete(ctx, objects)
	}
	collectionSetCache.Store([]string{})
	if !storageOnly {
		for _, b := range boards {
			if s.leaderboardCache != nil {
				s.leaderboardCache.Remove(b)
			}
		}
		for _, r := range ranks {
			if s.leaderboardRankCache != nil {
				s.leaderboardRankCache.DeleteLeaderboard(r.LeaderboardId, r.Expiry)
			}
		}
		if s.leaderboardScheduler != nil {
			s.leaderboardScheduler.Update()
		}
		for _, u := range users {
			if s.sessionCache != nil {
				s.sessionCache.RemoveAll(u)
			}
			if s.tracker != nil && s.sessionRegistry != nil {
				for _, p := range s.tracker.ListPresenceIDByStream(PresenceStream{Mode: StreamModeNotifications, Subject: u}) {
					if err := s.sessionRegistry.Disconnect(ctx, p.SessionID, false); err != nil {
						// The database commit has completed. Continue invalidating all
						// other users even if one local session cannot disconnect.
						s.logger.Warn("Could not disconnect session after console data reset", zap.Error(err))
					}
				}
			}
		}
		if s.tracker != nil {
			for _, g := range groups {
				s.tracker.UntrackByStream(PresenceStream{Mode: StreamModeGroup, Subject: g})
			}
		}
	}
	return &emptypb.Empty{}, nil
}
func (s *ConsoleServer) deleteAccountsMySQL(ctx context.Context) (*emptypb.Empty, error) {
	// The deprecated endpoint uses the complete account lifecycle, including caches.
	var last uuid.UUID
	for {
		rows, err := s.db.QueryContext(ctx, "SELECT id FROM users WHERE id > ? ORDER BY id LIMIT 100", last)
		if err != nil {
			return nil, err
		}
		ids := []uuid.UUID{}
		for rows.Next() {
			var id uuid.UUID
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if err = DeleteAccount(ctx, s.logger, s.db, s.config, s.leaderboardCache, s.leaderboardRankCache, s.sessionRegistry, s.sessionCache, s.tracker, id, false, s.storageIndex); err != nil {
				return nil, err
			}
			last = id
		}
	}
	return &emptypb.Empty{}, nil
}
