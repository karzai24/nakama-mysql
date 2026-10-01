// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
)

// Callers handle empty lists explicitly rather than generating IN ().
func mysqlPlaceholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func notificationsDeleteMySQL(ctx context.Context, db *sql.DB, owner string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	query := "DELETE FROM notification WHERE id IN (" + mysqlPlaceholders(len(ids)) + ")"
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	if owner != "" {
		query += " AND user_id = ?"
		args = append(args, owner)
	}
	_, err := db.ExecContext(ctx, query, args...)
	return err
}

func notificationsSaveMySQL(ctx context.Context, db *sql.DB, notifications map[uuid.UUID][]*api.Notification) error {
	owners := make([]uuid.UUID, 0, len(notifications))
	for owner := range notifications {
		owners = append(owners, owner)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i].String() < owners[j].String() })
	return executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		for _, owner := range owners {
			for _, n := range notifications[owner] {
				if _, err := tx.ExecContext(ctx, "INSERT INTO notification (id, user_id, subject, content, code, sender_id) VALUES (?, ?, ?, ?, ?, ?)", n.Id, owner.String(), n.Subject, n.Content, n.Code, n.SenderId); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func notificationsUpdateMySQL(ctx context.Context, db *sql.DB, updates []notificationUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	return executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		for _, update := range updates {
			var content any
			if update.Content != nil {
				encoded, err := json.Marshal(update.Content)
				if err != nil {
					return err
				}
				content = string(encoded)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE notification SET content = COALESCE(?, content), subject = COALESCE(?, subject), sender_id = COALESCE(?, sender_id) WHERE id = ?", content, update.Subject, update.Sender, update.Id.String()); err != nil {
				return err
			}
		}
		return nil
	})
}
