// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/console"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func consoleStorageQueryMySQL(in *console.ListStorageRequest, owner *uuid.UUID, c *consoleStorageCursor) (string, []any, int) {
	q := "SELECT collection, `key`, user_id, version, `read`, `write`, create_time, update_time FROM storage"
	args := []any{}
	where := []string{}
	order := "collection, `read`, `key`, user_id"
	limit := 100
	if in.Collection != "" {
		where = append(where, "collection = ?")
		args = append(args, in.Collection)
	}
	if owner != nil {
		where = append(where, "user_id = ?")
		args = append(args, *owner)
	}
	if in.Key != "" {
		op := " = ?"
		if isPrefixSearch(in.Key) {
			op = " LIKE ?"
		}
		where = append(where, "`key`"+op)
		args = append(args, in.Key)
	}
	if in.Collection == "" && owner != nil || in.Collection != "" && in.Key != "" && !isPrefixSearch(in.Key) && owner != nil {
		limit = 0
	}
	if in.Key != "" {
		order = "`key`, user_id"
		if c != nil {
			where = append(where, "(`key`,user_id) > (?,?)")
			args = append(args, c.Key, c.UserID)
		}
	} else if c != nil {
		where = append(where, "(collection,`read`,`key`,user_id) > (?,?,?,?)")
		args = append(args, c.Collection, c.Read, c.Key, c.UserID)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY " + order
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit+1)
	}
	return q, args, limit
}
func consoleGroupsQueryMySQL(filter string, c *consoleGroupCursor) (string, []any, int) {
	q := "SELECT id,creator_id,name,description,avatar_url,state,edge_count,lang_tag,max_count,metadata,create_time,update_time FROM `groups`"
	args := []any{}
	limit := 50
	if id, err := uuid.FromString(filter); filter != "" && err == nil {
		return q + " WHERE id = ?", []any{id}, 0
	}
	if filter != "" {
		if !strings.Contains(filter, "%") {
			return q + " WHERE name = ?", []any{filter}, 0
		}
		q += " WHERE LOWER(name) LIKE LOWER(?)"
		args = append(args, filter)
		if c != nil {
			q += " AND name > ?"
			args = append(args, c.Name)
		}
		q += " ORDER BY name"
	} else {
		if c != nil {
			q += " WHERE id > ?"
			args = append(args, c.ID)
		}
		q += " ORDER BY id"
	}
	q += " LIMIT ?"
	args = append(args, limit+1)
	return q, args, limit
}
func consoleNotificationsQueryMySQL(in *console.ListNotificationsRequest, c *notificationsCursor) (string, []any, error) {
	if in.Limit < 1 || in.Limit > 100 {
		return "", nil, status.Error(codes.InvalidArgument, "Limit must be between 1 and 100.")
	}
	q := "SELECT id,user_id,subject,content,code,sender_id,create_time FROM notification"
	where := []string{}
	args := []any{}
	order := "user_id,create_time,id"
	if in.UserId != "" {
		id, err := uuid.FromString(in.UserId)
		if err != nil {
			return "", nil, status.Error(codes.InvalidArgument, "Invalid user ID.")
		}
		if c != nil && uuid.FromBytesOrNil(c.UserID) != id {
			return "", nil, status.Error(codes.InvalidArgument, "Cursor does not match user.")
		}
		where = append(where, "user_id = ?")
		args = append(args, id)
	}
	if c != nil {
		owner, e1 := uuid.FromBytes(c.UserID)
		id, e2 := uuid.FromBytes(c.NotificationID)
		if e1 != nil || e2 != nil {
			return "", nil, status.Error(codes.InvalidArgument, "Invalid cursor.")
		}
		op := ">"
		if !c.IsNext {
			op = "<"
			order = "user_id DESC,create_time DESC,id DESC"
		}
		where = append(where, "(user_id,create_time,id) "+op+" (?,?,?)")
		args = append(args, owner, time.Unix(0, c.CreateTime).UTC(), id)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY " + order + " LIMIT ?"
	args = append(args, in.Limit+1)
	return q, args, nil
}
func countConsoleRecords(ctx context.Context, logger *zap.Logger, db *sql.DB, table string) int32 {
	// Only internal table names reach SQL identifiers.
	switch table {
	case "users", "user_tombstone", "storage", "groups":
	default:
		return 0
	}
	var n int64
	query := "SELECT COUNT(*) FROM " + table
	{
		query = "SELECT COUNT(*) FROM `" + table + "`"
	}
	if err := db.QueryRowContext(ctx, query).Scan(&n); err != nil {
		logger.Warn("Could not count console records", zap.Error(err))
		return 0
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}
