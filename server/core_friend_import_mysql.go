// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofrs/uuid/v5"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func providerFriendIDsMySQL(ctx context.Context, tx *sql.Tx, column string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows *sql.Rows
	var err error
	if strings.HasPrefix(column, "registered:") {
		args := append([]any{strings.TrimPrefix(column, "registered:")}, mysqlStringArgs(ids)...)
		rows, err = tx.QueryContext(ctx, "SELECT user_id FROM user_device WHERE provider = ? AND id IN ("+mysqlMarks(len(ids))+")", args...)
	} else {
		if column != "facebook_id" && column != "steam_id" {
			return nil, errors.New("unsupported friend import provider")
		}
		rows, err = tx.QueryContext(ctx, "SELECT id FROM users WHERE "+column+" IN ("+mysqlMarks(len(ids))+")", mysqlStringArgs(ids)...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		users = append(users, id)
	}
	return users, rows.Err()
}

func resetFriendPeersMySQL(ctx context.Context, tx *sql.Tx, owner uuid.UUID) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT destination_id FROM user_edge WHERE source_id = ? AND state <> 3 UNION SELECT source_id FROM user_edge WHERE destination_id = ? AND state <> 3", owner, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	peers := []string{}
	for rows.Next() {
		var peer string
		if err := rows.Scan(&peer); err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	return peers, rows.Err()
}

// Called only with owner and peer user rows locked. Preserve blocks in either
// direction and decrement the actual source of every removed edge.
func resetFriendPairMySQL(ctx context.Context, tx *sql.Tx, owner, peer string) error {
	for _, pair := range [][2]string{{owner, peer}, {peer, owner}} {
		result, err := tx.ExecContext(ctx, "DELETE FROM user_edge WHERE source_id = ? AND destination_id = ? AND state <> 3", pair[0], pair[1])
		if err != nil {
			return err
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if removed != 0 {
			if err := friendCountMySQL(ctx, tx, pair[0], -int(removed)); err != nil {
				return err
			}
		}
	}
	return nil
}

func importProviderFriendsMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, router MessageRouter, owner uuid.UUID, username, column string, providerIDs []string, provider string, reset bool) error {
	var imported []uuid.UUID
	value, _ := json.Marshal(map[string]string{"provider": strings.ToLower(provider)})
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		imported = nil
		if err := lockFriendOwnerMySQL(ctx, tx, owner); err != nil {
			return err
		}
		candidates, err := providerFriendIDsMySQL(ctx, tx, column, providerIDs)
		if err != nil {
			return err
		}
		targets := append([]string{}, candidates...)
		var resetPeers []string
		if reset {
			resetPeers, err = resetFriendPeersMySQL(ctx, tx, owner)
			if err != nil {
				return err
			}
			targets = append(targets, resetPeers...)
		}
		locked, err := lockFriendPeersMySQL(ctx, tx, owner, targets)
		if err != nil {
			return err
		}
		lockedSet := map[string]bool{}
		for _, id := range locked {
			lockedSet[id] = true
		}
		// Recheck provider ownership after locks: a link/unlink may have committed
		// while we waited. Accounts newly matching after the snapshot wait until
		// the next import instead of being mutated without an ownership lock.
		candidates, err = providerFriendIDsMySQL(ctx, tx, column, providerIDs)
		if err != nil {
			return err
		}
		if reset {
			for _, peer := range resetPeers {
				if lockedSet[peer] {
					if err := resetFriendPairMySQL(ctx, tx, owner.String(), peer); err != nil {
						return err
					}
				}
			}
		}
		for _, peer := range candidates {
			if !lockedSet[peer] {
				continue
			}
			changed, _, err := mutateFriendPairMySQL(ctx, tx, owner.String(), peer, "import", string(value))
			if err != nil {
				return err
			}
			if changed {
				imported = append(imported, uuid.Must(uuid.FromString(peer)))
			}
		}
		return nil
	})
	if err != nil {
		logger.Error("Error importing MySQL provider friends.", zap.Error(err))
		return status.Error(codes.Internal, "Error importing provider friends.")
	}
	if len(imported) > 0 {
		sendFriendAddedNotification(ctx, logger, db, tracker, router, owner, username, imported)
	}
	return nil
}
