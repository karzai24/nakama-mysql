// Modified by nakama-mysql contributors: native MySQL storage dispatch.
// Copyright 2018 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/base64"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"

	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type storageCursor struct {
	Key    string
	UserID uuid.UUID
	Read   int32
}

// Internal representation for a batch of storage write operations.
type StorageOpWrites []*StorageOpWrite

type StorageOpWrite struct {
	OwnerID string
	Object  *api.WriteStorageObject
}

// Desired `read` persmission after this Op completes
func (op *StorageOpWrite) permissionRead() int32 {
	if op.Object.PermissionRead != nil {
		return op.Object.PermissionRead.Value
	}
	return 1
}

// Desired `write` persmission after this Op completes
func (op *StorageOpWrite) permissionWrite() int32 {
	if op.Object.PermissionWrite != nil {
		return op.Object.PermissionWrite.Value
	}
	return 1
}

// Expected object version after this Op completes
func (op *StorageOpWrite) expectedVersion() string {
	hash := md5.Sum([]byte(op.Object.Value))
	return hex.EncodeToString(hash[:])
}

func (s StorageOpWrites) Len() int {
	return len(s)
}
func (s StorageOpWrites) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s StorageOpWrites) Less(i, j int) bool {
	s1, s2 := s[i], s[j]
	if s1.Object.Collection != s2.Object.Collection {
		return s1.Object.Collection < s2.Object.Collection
	}
	if s1.Object.Key != s2.Object.Key {
		return s1.Object.Key < s2.Object.Key
	}
	return s1.OwnerID < s2.OwnerID
}

// Internal representation for a batch of storage delete operations.
type StorageOpDeletes []*StorageOpDelete

type StorageOpDelete struct {
	OwnerID  string
	ObjectID *api.DeleteStorageObjectId
}

func (s StorageOpDeletes) Len() int {
	return len(s)
}
func (s StorageOpDeletes) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s StorageOpDeletes) Less(i, j int) bool {
	s1, s2 := s[i], s[j]
	if s1.ObjectID.Collection != s2.ObjectID.Collection {
		return s1.ObjectID.Collection < s2.ObjectID.Collection
	}
	if s1.ObjectID.Key != s2.ObjectID.Key {
		return s1.ObjectID.Key < s2.ObjectID.Key
	}
	return s1.OwnerID < s2.OwnerID
}

func StorageListObjects(ctx context.Context, logger *zap.Logger, db *sql.DB, caller uuid.UUID, ownerID *uuid.UUID, collection string, limit int, cursor string) (*api.StorageObjectList, codes.Code, error) {
	if limit <= 0 {
		return &api.StorageObjectList{Objects: make([]*api.StorageObject, 0), Cursor: ""}, codes.OK, nil
	}

	var sc *storageCursor
	if cursor != "" {
		sc = &storageCursor{}
		cb, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			logger.Warn("Could not base64 decode storage cursor.", zap.String("cursor", cursor))
			return nil, codes.InvalidArgument, errors.New("Malformed cursor was used.")
		}
		if err := gob.NewDecoder(bytes.NewReader(cb)).Decode(sc); err != nil {
			logger.Warn("Could not decode storage cursor.", zap.String("cursor", cursor))
			return nil, codes.InvalidArgument, errors.New("Malformed cursor was used.")
		}
	}

	var result *api.StorageObjectList
	var resultErr error

	if caller == uuid.Nil {
		// Call from the runtime.
		if ownerID == nil {
			// List storage regardless of user.
			result, resultErr = StorageListObjectsAll(ctx, logger, db, true, collection, limit, cursor, sc)
		} else {
			// List for a particular user ID.
			result, resultErr = StorageListObjectsUser(ctx, logger, db, true, *ownerID, collection, limit, cursor, sc)
		}
	} else {
		// Call from a client.
		if ownerID == nil {
			// List publicly readable storage regardless of owner.
			result, resultErr = StorageListObjectsAll(ctx, logger, db, false, collection, limit, cursor, sc)
		} else if o := *ownerID; caller == o {
			// User listing their own data.
			result, resultErr = StorageListObjectsUser(ctx, logger, db, false, o, collection, limit, cursor, sc)
		} else {
			// User listing someone else's data.
			result, resultErr = StorageListObjectsPublicReadUser(ctx, logger, db, o, collection, limit, cursor, sc)
		}
	}

	if resultErr != nil {
		return nil, codes.Internal, resultErr
	}

	if cursor != "" && result.Cursor == cursor {
		result.Cursor = ""
	}
	return result, codes.OK, nil
}

func StorageListObjectsAll(ctx context.Context, logger *zap.Logger, db *sql.DB, authoritative bool, collection string, limit int, cursor string, storageCursor *storageCursor) (*api.StorageObjectList, error) {

	return storageListMySQL(ctx, db, authoritative, nil, false, collection, limit, storageCursor)

}

func StorageListObjectsPublicReadUser(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, collection string, limit int, cursor string, storageCursor *storageCursor) (*api.StorageObjectList, error) {

	return storageListMySQL(ctx, db, false, &userID, true, collection, limit, storageCursor)

}

func StorageListObjectsUser(ctx context.Context, logger *zap.Logger, db *sql.DB, authoritative bool, userID uuid.UUID, collection string, limit int, cursor string, storageCursor *storageCursor) (*api.StorageObjectList, error) {

	return storageListMySQL(ctx, db, authoritative, &userID, false, collection, limit, storageCursor)

}

func StorageReadAllUserObjects(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID) ([]*api.StorageObject, error) {
	{
		rows, err := db.QueryContext(ctx, "SELECT "+mysqlStorageColumns+" FROM storage WHERE user_id = ? ORDER BY collection, `key`", userID.String())
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		objects := make([]*api.StorageObject, 0)
		for rows.Next() {
			object, err := scanStorageMySQL(rows)
			if err != nil {
				return nil, err
			}
			objects = append(objects, object)
		}
		return objects, rows.Err()
	}

}

func storageListObjects(rows *sql.Rows, limit int) (*api.StorageObjectList, error) {
	var lastObject *api.StorageObject
	var newCursor *storageCursor
	objects := make([]*api.StorageObject, 0, limit)
	for rows.Next() {
		// If we've read enough, but there is at least 1 more, use the last read as the cursor and stop here.
		if len(objects) >= limit && lastObject != nil {
			newCursor = &storageCursor{
				Key:  lastObject.Key,
				Read: lastObject.PermissionRead,
			}
			if lastObject.UserId != "" {
				newCursor.UserID = uuid.FromStringOrNil(lastObject.UserId)
			}
			break
		}

		// There is still room for more objects, read the next one.
		o := &api.StorageObject{CreateTime: &timestamppb.Timestamp{}, UpdateTime: &timestamppb.Timestamp{}}
		var createTime sql.NullTime
		var updateTime sql.NullTime

		if err := rows.Scan(&o.Collection, &o.Key, &o.UserId, &o.Value, &o.Version, &o.PermissionRead, &o.PermissionWrite, &createTime, &updateTime); err != nil {
			_ = rows.Close()
			return nil, err
		}

		o.CreateTime.Seconds = createTime.Time.Unix()
		o.UpdateTime.Seconds = updateTime.Time.Unix()

		objects = append(objects, o)
		lastObject = o
	}
	_ = rows.Close()

	if rows.Err() != nil {
		return nil, rows.Err()
	}

	// Prepare the response and include the cursor, if any.
	objectList := &api.StorageObjectList{
		Objects: objects,
	}
	if newCursor != nil {
		cursorBuf := new(bytes.Buffer)
		if err := gob.NewEncoder(cursorBuf).Encode(newCursor); err != nil {
			return nil, err
		}
		objectList.Cursor = base64.RawURLEncoding.EncodeToString(cursorBuf.Bytes())
	}

	return objectList, nil
}

type storageQueryArg struct {
	name   string
	dbType string
	param  any
}

func StorageReadObjects(ctx context.Context, logger *zap.Logger, db *sql.DB, caller uuid.UUID, objectIDs []*api.ReadStorageObjectId) (*api.StorageObjects, error) {

	return storageReadMySQL(ctx, db, caller, objectIDs)

}

func StorageWriteObjects(ctx context.Context, logger *zap.Logger, db *sql.DB, metrics Metrics, storageIndex StorageIndex, authoritativeWrite bool, ops StorageOpWrites) (*api.StorageObjectAcks, codes.Code, error) {

	return storageWriteMySQL(ctx, logger, db, metrics, storageIndex, authoritativeWrite, ops)

}

func StorageWriteWithRetries(ctx context.Context, logger *zap.Logger, db *sql.DB, metrics Metrics, storageIndex StorageIndex, objectIDs []*api.ReadStorageObjectId, updateFn func(objects []*api.StorageObject) ([]*runtime.StorageWrite, error), maxRetries int) (*api.StorageObjectAcks, error) {
	for retryCount := 0; retryCount <= maxRetries; retryCount++ {
		objs, err := StorageReadObjects(ctx, logger, db, uuid.Nil, objectIDs)
		if err != nil {
			return nil, err
		}

		writes, err := updateFn(objs.Objects)
		if err != nil {
			return nil, err
		}

		ops := make(StorageOpWrites, 0, len(writes))
		for _, write := range writes {
			if write.Collection == "" {
				return nil, errors.New("write error: expects collection to be a non-empty string")
			}
			if write.Key == "" {
				return nil, errors.New("write error: expects key to be a non-empty string")
			}
			if write.UserID != "" {
				if _, err := uuid.FromString(write.UserID); err != nil {
					return nil, errors.New("write error: expects an empty or valid user id")
				}
			}
			if maybeJSON := []byte(write.Value); !json.Valid(maybeJSON) || bytes.TrimSpace(maybeJSON)[0] != byteBracket {
				return nil, errors.New("write error: value must be a JSON-encoded object")
			}

			op := &StorageOpWrite{
				Object: &api.WriteStorageObject{
					Collection:      write.Collection,
					Key:             write.Key,
					Value:           write.Value,
					Version:         write.Version,
					PermissionRead:  &wrapperspb.Int32Value{Value: int32(write.PermissionRead)},
					PermissionWrite: &wrapperspb.Int32Value{Value: int32(write.PermissionWrite)},
				},
			}
			if write.UserID == "" {
				op.OwnerID = uuid.Nil.String()
			} else {
				op.OwnerID = write.UserID
			}

			ops = append(ops, op)
		}

		acks, _, err := StorageWriteObjects(ctx, logger, db, metrics, storageIndex, true, ops)
		if err != nil {
			if errors.Is(err, runtime.ErrStorageRejectedVersion) {
				backoff := min(int64(2<<retryCount)*int64(time.Millisecond), int64(20*time.Millisecond))
				delay := time.Duration(backoff) + time.Duration(rand.Int64N(10))*time.Millisecond
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
				}
				continue
			}
			return nil, err
		}

		return acks, nil
	}

	return nil, runtime.ErrStorageWriteExhaustedRetries
}

func StorageDeleteObjects(ctx context.Context, logger *zap.Logger, db *sql.DB, storageIndex StorageIndex, authoritativeDelete bool, ops StorageOpDeletes) (codes.Code, error) {

	return storageDeleteMySQL(ctx, logger, db, storageIndex, authoritativeDelete, ops)

}

func storageIndexWrite(ctx context.Context, storageIndex StorageIndex, ops StorageOpWrites, acks []*api.StorageObjectAck) {
	storageIndex.Write(ctx, storageIndexObjects(ops, acks))
}

// Acknowledgements are in caller order, so pair them with the original operations,
// never the independently sorted SQL execution order.
func storageIndexObjects(ops StorageOpWrites, acks []*api.StorageObjectAck) []*api.StorageObject {
	sw := make([]*api.StorageObject, 0, len(ops))
	for i, o := range ops {
		sw = append(sw, &api.StorageObject{
			Collection:      o.Object.Collection,
			Key:             o.Object.Key,
			UserId:          o.OwnerID,
			Value:           o.Object.Value,
			Version:         acks[i].Version,
			PermissionRead:  o.Object.PermissionRead.GetValue(),
			PermissionWrite: o.Object.PermissionWrite.GetValue(),
			CreateTime:      acks[i].CreateTime,
			UpdateTime:      acks[i].UpdateTime,
		})
	}

	return sw
}
