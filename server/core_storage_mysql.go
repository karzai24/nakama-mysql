// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const mysqlStorageColumns = "collection, `key`, user_id, value, version, `read`, `write`, create_time, update_time"

func storageListMySQL(ctx context.Context, db *sql.DB, authoritative bool, owner *uuid.UUID, publicOwner bool, collection string, limit int, cursor *storageCursor) (*api.StorageObjectList, error) {
	query := "SELECT " + mysqlStorageColumns + " FROM storage WHERE collection = ?"
	args := []any{collection}
	if owner != nil {
		query += " AND user_id = ?"
		args = append(args, owner.String())
	}
	switch {
	case publicOwner:
		query += " AND `read` = 2"
	case !authoritative && owner == nil:
		query += " AND `read` >= 2"
	case !authoritative:
		query += " AND `read` >= 1"
	}
	if cursor != nil {
		switch {
		case publicOwner:
			query += " AND `key` > ?"
			args = append(args, cursor.Key)
		case owner != nil:
			query += " AND (`read`, `key`) > (?, ?)"
			args = append(args, cursor.Read, cursor.Key)
		default:
			read := cursor.Read
			if !authoritative {
				read = 2
			}
			query += " AND (`read`, `key`, user_id) > (?, ?, ?)"
			args = append(args, read, cursor.Key, cursor.UserID.String())
		}
	}
	if publicOwner {
		query += " ORDER BY `key`"
	} else {
		query += " ORDER BY `read`, `key`, user_id"
	}
	query += " LIMIT ?"
	args = append(args, limit+1)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return storageListObjects(rows, limit)
}

func storageReadMySQL(ctx context.Context, db *sql.DB, caller uuid.UUID, ids []*api.ReadStorageObjectId) (*api.StorageObjects, error) {
	objects := make([]*api.StorageObject, 0, len(ids))
	if len(ids) == 0 {
		return &api.StorageObjects{Objects: objects}, nil
	}
	predicates := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids)*3+1)
	for _, id := range ids {
		owner := uuid.Nil
		if id.UserId != "" {
			var err error
			owner, err = uuid.FromString(id.UserId)
			if err != nil {
				return nil, err
			}
		}
		predicates = append(predicates, "(collection = ? AND `key` = ? AND user_id = ?)")
		args = append(args, id.Collection, id.Key, owner.String())
	}
	query := "SELECT " + mysqlStorageColumns + " FROM storage WHERE (" + strings.Join(predicates, " OR ") + ")"
	if caller != uuid.Nil {
		query += " AND (`read` = 2 OR (`read` = 1 AND user_id = ?))"
		args = append(args, caller.String())
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		object, err := scanStorageMySQL(rows)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return &api.StorageObjects{Objects: objects}, rows.Err()
}

func scanStorageMySQL(row Scannable) (*api.StorageObject, error) {
	o := &api.StorageObject{}
	var created, updated time.Time
	if err := row.Scan(&o.Collection, &o.Key, &o.UserId, &o.Value, &o.Version, &o.PermissionRead, &o.PermissionWrite, &created, &updated); err != nil {
		return nil, err
	}
	o.CreateTime, o.UpdateTime = timestamppb.New(created), timestamppb.New(updated)
	return o, nil
}

// Lock owners in a stable order, including for rows that do not exist yet. This
// deliberately serializes storage writes per owner in the first port. It
// avoids gap-lock and insert races under READ COMMITTED without placeholder rows.
func lockStorageOwnersMySQL(ctx context.Context, tx *sql.Tx, owners []string) error {
	unique := make(map[string]struct{}, len(owners))
	for _, owner := range owners {
		unique[owner] = struct{}{}
	}
	sorted := make([]string, 0, len(unique))
	for owner := range unique {
		sorted = append(sorted, owner)
	}
	sort.Strings(sorted)
	for _, owner := range sorted {
		var id string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", owner).Scan(&id); err != nil {
			return err
		}
	}
	return nil
}

func storageWriteMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, metrics Metrics, index StorageIndex, authoritative bool, ops StorageOpWrites) (*api.StorageObjectAcks, codes.Code, error) {
	var acks []*api.StorageObjectAck
	var indexedObjects []*api.StorageObject
	var rejectedCollection string
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		var err error
		acks, indexedObjects, rejectedCollection, err = storageWriteTxMySQL(ctx, tx, authoritative, ops)
		return err
	})
	if err != nil {
		reason := ""
		if errors.Is(err, runtime.ErrStorageRejectedVersion) {
			reason = "version"
		}
		if errors.Is(err, runtime.ErrStorageRejectedPermission) {
			reason = "permission"
		}
		if reason != "" {
			metrics.StorageWriteRejectCount(map[string]string{"collection": rejectedCollection, "reason": reason}, 1)
			return nil, codes.InvalidArgument, err
		}
		logger.Error("MySQL storage write failed", zap.Error(err))
		return nil, codes.Internal, err
	}
	index.Write(ctx, indexedObjects)
	return &api.StorageObjectAcks{Acks: acks}, codes.OK, nil
}

// Only SQL runs here. The caller publishes index effects after its entire commit.
func storageWriteTxMySQL(ctx context.Context, tx *sql.Tx, authoritative bool, ops StorageOpWrites) ([]*api.StorageObjectAck, []*api.StorageObject, string, error) {
	type indexedOp struct {
		index int
		op    *StorageOpWrite
	}
	sorted := make([]indexedOp, len(ops))
	owners := make([]string, len(ops))
	for i, op := range ops {
		owner, err := uuid.FromString(op.OwnerID)
		if err != nil {
			return nil, nil, "", err
		}
		normalized := &StorageOpWrite{OwnerID: owner.String(), Object: op.Object}
		sorted[i] = indexedOp{i, normalized}
		owners[i] = normalized.OwnerID
	}
	sort.SliceStable(sorted, func(i, j int) bool { return (StorageOpWrites{sorted[i].op, sorted[j].op}).Less(0, 1) })
	var rejectedCollection string
	acks := make([]*api.StorageObjectAck, len(ops))
	indexedObjects := make([]*api.StorageObject, 0, len(ops))
	if err := lockStorageOwnersMySQL(ctx, tx, owners); err != nil {
		return nil, nil, rejectedCollection, err
	}
	for _, entry := range sorted {
		op, object := entry.op, entry.op.Object
		rejectedCollection = object.Collection
		current, err := scanStorageMySQL(tx.QueryRowContext(ctx, "SELECT "+mysqlStorageColumns+" FROM storage WHERE collection = ? AND `key` = ? AND user_id = ? FOR UPDATE", object.Collection, object.Key, op.OwnerID))
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, rejectedCollection, err
		}
		if object.Version == "*" && exists {
			return nil, nil, rejectedCollection, runtime.ErrStorageRejectedVersion
		}
		if exists && !authoritative && current.PermissionWrite != 1 {
			return nil, nil, rejectedCollection, runtime.ErrStorageRejectedPermission
		}
		if object.Version != "" && object.Version != "*" && (!exists || current.Version != object.Version) {
			return nil, nil, rejectedCollection, runtime.ErrStorageRejectedVersion
		}
		version, read, write := op.expectedVersion(), op.permissionRead(), op.permissionWrite()
		if !exists {
			_, err = tx.ExecContext(ctx, "INSERT INTO storage (collection, `key`, user_id, value, version, `read`, `write`) VALUES (?, ?, ?, ?, ?, ?, ?)", object.Collection, object.Key, op.OwnerID, object.Value, version, read, write)
		} else if object.Version != "" || current.Version != version || current.PermissionRead != read || current.PermissionWrite != write {
			_, err = tx.ExecContext(ctx, "UPDATE storage SET value = ?, version = ?, `read` = ?, `write` = ?, update_time = CURRENT_TIMESTAMP(6) WHERE collection = ? AND `key` = ? AND user_id = ?", object.Value, version, read, write, object.Collection, object.Key, op.OwnerID)
		}
		if err != nil {
			if mysqlErrorNumber(err, 1062) {
				return nil, nil, rejectedCollection, runtime.ErrStorageRejectedVersion
			}
			return nil, nil, rejectedCollection, err
		}
		stored, err := scanStorageMySQL(tx.QueryRowContext(ctx, "SELECT "+mysqlStorageColumns+" FROM storage WHERE collection = ? AND `key` = ? AND user_id = ?", object.Collection, object.Key, op.OwnerID))
		if err != nil {
			return nil, nil, rejectedCollection, err
		}
		acks[entry.index] = &api.StorageObjectAck{Collection: object.Collection, Key: object.Key, UserId: op.OwnerID, Version: stored.Version, CreateTime: stored.CreateTime, UpdateTime: stored.UpdateTime}
		indexedObjects = append(indexedObjects, stored)
	}
	return acks, indexedObjects, rejectedCollection, nil
}

var errStorageDeleteMySQL = errors.New("Storage delete rejected - not found, version check failed, or permission denied.")

func storageDeleteMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, index StorageIndex, authoritative bool, ops StorageOpDeletes) (codes.Code, error) {
	var sorted StorageOpDeletes
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		var err error
		sorted, err = storageDeleteTxMySQL(ctx, tx, authoritative, ops)
		return err
	})
	if err != nil {
		if errors.Is(err, errStorageDeleteMySQL) {
			return codes.InvalidArgument, err
		}
		logger.Error("MySQL storage delete failed", zap.Error(err))
		return codes.Internal, err
	}
	index.Delete(ctx, sorted)
	return codes.OK, nil
}

func storageDeleteTxMySQL(ctx context.Context, tx *sql.Tx, authoritative bool, ops StorageOpDeletes) (StorageOpDeletes, error) {
	sorted := make(StorageOpDeletes, len(ops))
	for i, op := range ops {
		owner, err := uuid.FromString(op.OwnerID)
		if err != nil {
			return nil, err
		}
		sorted[i] = &StorageOpDelete{OwnerID: owner.String(), ObjectID: op.ObjectID}
	}
	sort.Sort(sorted)
	for _, op := range sorted {
		args := []any{op.ObjectID.Collection, op.ObjectID.Key, op.OwnerID}
		query := "DELETE FROM storage WHERE collection = ? AND `key` = ? AND user_id = ?"
		if !authoritative {
			query += " AND `write` > 0"
		}
		if op.ObjectID.Version != "" {
			query += " AND version = ?"
			args = append(args, op.ObjectID.Version)
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count == 0 && (!authoritative || op.ObjectID.Version != "") {
			return nil, errStorageDeleteMySQL
		}
	}
	return sorted, nil
}
