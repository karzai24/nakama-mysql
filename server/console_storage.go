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
	"database/sql"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"

	"strings"
	"sync/atomic"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/console"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type consoleStorageCursor struct {
	Key        string
	UserID     uuid.UUID
	Collection string
	Read       int32
}

var collectionSetCache = &atomic.Value{}

func (s *ConsoleServer) DeleteStorage(ctx context.Context, in *emptypb.Empty) (*emptypb.Empty, error) {

	return s.clearDataMySQL(ctx, true)

}

func (s *ConsoleServer) DeleteStorageObject(ctx context.Context, in *console.DeleteStorageObjectRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	if in.Collection == "" {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid collection.")
	}
	if in.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid key.")
	}
	_, err := uuid.FromString(in.UserId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	code, err := StorageDeleteObjects(ctx, logger, s.db, s.storageIndex, true, StorageOpDeletes{
		&StorageOpDelete{
			OwnerID: in.UserId,
			ObjectID: &api.DeleteStorageObjectId{
				Collection: in.Collection,
				Key:        in.Key,
				Version:    in.Version,
			},
		},
	})

	if err != nil {
		if code == codes.Internal {
			logger.Error("Failed to delete storage object.", zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while deleting storage object.")
		}

		// OCC error or storage not found, no need to log.
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) GetStorage(ctx context.Context, in *api.ReadStorageObjectId) (*api.StorageObject, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	if in.Collection == "" {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid collection.")
	}
	if in.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid key.")
	}
	_, err := uuid.FromString(in.UserId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	objects, err := StorageReadObjects(ctx, logger, s.db, uuid.Nil, []*api.ReadStorageObjectId{in})
	if err != nil {
		// Errors already logged in function above.
		return nil, status.Error(codes.Internal, "An error occurred while reading storage object.")
	}

	if len(objects.Objects) == 0 {
		// Not found.
		return nil, status.Error(codes.NotFound, "Storage object not found.")
	}

	return objects.Objects[0], nil
}

func (s *ConsoleServer) ListStorageCollections(ctx context.Context, in *emptypb.Empty) (*console.StorageCollectionsList, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	collectionSetCache := collectionSetCache.Load()
	if collectionSetCache == nil {
		return &console.StorageCollectionsList{
			Collections: make([]string, 0),
		}, nil
	}

	collections, ok := collectionSetCache.([]string)
	if !ok {
		logger.Error("Error reading collection set cache, not a []string.")
		return &console.StorageCollectionsList{
			Collections: make([]string, 0),
		}, nil
	}

	return &console.StorageCollectionsList{
		Collections: collections,
	}, nil
}

func (s *ConsoleServer) ListStorage(ctx context.Context, in *console.ListStorageRequest) (*console.StorageList, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)

	// Validate user ID, if provided.
	var userID *uuid.UUID
	if in.UserId != "" {
		uid, err := uuid.FromString(in.UserId)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID when provided.")
		}
		userID = &uid
	}

	// Validate cursor, if provided.
	var cursor *consoleStorageCursor
	if in.Cursor != "" {
		// Pagination not allowed when filtering only by user ID. Don't process the cursor further.
		if in.Collection == "" && in.Key == "" && userID != nil {
			return nil, status.Error(codes.InvalidArgument, "Cursor not allowed when filter only contains user ID.")
		}
		// Pagination not allowed when filtering by collection, key, and user ID all at once. Don't process the cursor further.
		if in.Collection != "" && in.Key != "" && userID != nil && !isPrefixSearch(in.Key) {
			return nil, status.Error(codes.InvalidArgument, "Cursor not allowed when filter only contains collection, key, and user ID.")
		}

		cb, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil {
			logger.Warn("Could not base64 decode console storage list cursor.", zap.String("cursor", in.Cursor))
			return nil, status.Error(codes.InvalidArgument, "Malformed cursor was used.")
		}
		cursor = &consoleStorageCursor{}
		if err := gob.NewDecoder(bytes.NewReader(cb)).Decode(cursor); err != nil {
			logger.Error("Error decoding console storage list cursor.", zap.String("cursor", in.Cursor), zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to decode console storage list request cursor.")
		}

		// If cursor was provided it must not clash with filter parameters.
		if in.Collection != "" && in.Collection != cursor.Collection {
			return nil, status.Error(codes.InvalidArgument, "Requires a matching cursor and collection filter property.")
		}
		if in.Key != "" && (!isPrefixSearch(in.Key) && in.Key != cursor.Key) {
			return nil, status.Error(codes.InvalidArgument, "Requires a matching cursor and key filter property.")
		}
		if in.UserId != "" && in.UserId != cursor.UserID.String() {
			return nil, status.Error(codes.InvalidArgument, "Requires a matching cursor and user ID filter property.")
		}
	}

	if in.Collection == "" && in.Key != "" {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid combination of filters.")
	}
	query, params, limit := consoleStorageQueryMySQL(in, userID, cursor)
	rows, err := s.db.QueryContext(ctx, query, params...)
	if err != nil {
		logger.Error("Error querying storage objects.", zap.Any("in", in), zap.Error(err))
		return nil, status.Error(codes.Internal, "An error occurred while trying to list storage objects.")
	}

	objects := make([]*console.StorageListObject, 0, 100)
	var nextCursor *consoleStorageCursor
	var previousObj *console.StorageListObject

	for rows.Next() {
		// Check limit before processing for the use case where (last page == limit) => null cursor.
		if limit > 0 && len(objects) >= limit {
			nextCursor = &consoleStorageCursor{
				Key:        previousObj.Key,
				UserID:     uuid.FromStringOrNil(previousObj.UserId),
				Collection: previousObj.Collection,
				Read:       previousObj.PermissionRead,
			}
			break
		}

		o := &console.StorageListObject{CreateTime: &timestamppb.Timestamp{}, UpdateTime: &timestamppb.Timestamp{}}
		var createTime sql.NullTime
		var updateTime sql.NullTime

		if err := rows.Scan(&o.Collection, &o.Key, &o.UserId, &o.Version, &o.PermissionRead, &o.PermissionWrite, &createTime, &updateTime); err != nil {
			_ = rows.Close()
			logger.Error("Error scanning storage objects.", zap.Any("in", in), zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to list storage objects.")
		}

		o.CreateTime.Seconds = createTime.Time.Unix()
		o.UpdateTime.Seconds = updateTime.Time.Unix()

		objects = append(objects, o)
		previousObj = o
	}
	_ = rows.Close()

	if err := rows.Err(); err != nil {
		return nil, err
	}

	response := &console.StorageList{
		Objects:    objects,
		TotalCount: countDatabase(ctx, logger, s.db, "storage"),
	}

	if nextCursor != nil {
		cursorBuf := &bytes.Buffer{}
		if err := gob.NewEncoder(cursorBuf).Encode(nextCursor); err != nil {
			logger.Error("Error encoding storage cursor.", zap.Any("in", in), zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to list storage objects.")
		}
		response.NextCursor = base64.RawURLEncoding.EncodeToString(cursorBuf.Bytes())
	}

	return response, nil
}

func (s *ConsoleServer) WriteStorageObject(ctx context.Context, in *console.WriteStorageObjectRequest) (*api.StorageObjectAck, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	if in.Collection == "" {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid collection.")
	}
	if in.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid key.")
	}
	_, err := uuid.FromString(in.UserId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	if in.PermissionRead != nil {
		permissionRead := in.PermissionRead.GetValue()
		if permissionRead < 0 || permissionRead > 2 {
			return nil, status.Error(codes.InvalidArgument, "Requires a valid read permission read if supplied (0-2).")
		}
	}
	if in.PermissionWrite != nil {
		permissionWrite := in.PermissionWrite.GetValue()
		if permissionWrite < 0 || permissionWrite > 1 {
			return nil, status.Error(codes.InvalidArgument, "Requires a valid write permission if supplied (0-1).")
		}
	}

	if maybeJSON := []byte(in.Value); !json.Valid(maybeJSON) || bytes.TrimSpace(maybeJSON)[0] != byteBracket {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid JSON object value.")
	}

	acks, code, err := StorageWriteObjects(ctx, logger, s.db, s.metrics, s.storageIndex, true, StorageOpWrites{
		&StorageOpWrite{
			OwnerID: in.UserId,
			Object: &api.WriteStorageObject{
				Collection:      in.Collection,
				Key:             in.Key,
				Value:           in.Value,
				Version:         in.Version,
				PermissionRead:  in.PermissionRead,
				PermissionWrite: in.PermissionWrite,
			},
		},
	})

	if err != nil {
		if code == codes.Internal {
			logger.Error("Failed to write storage object.", zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while writing storage object.")
		}

		// OCC error, no need to log.
		return nil, err
	}

	if acks == nil || len(acks.Acks) < 1 {
		logger.Error("Failed to get storage object acks.")
		return nil, status.Error(codes.Internal, "An error occurred while writing storage object.")
	}

	return acks.Acks[0], nil
}

func countDatabase(ctx context.Context, logger *zap.Logger, db *sql.DB, tableName string) int32 {
	return countConsoleRecords(ctx, logger, db, tableName)
}

func isPrefixSearch(key string) bool {
	return strings.HasSuffix(key, "%") && strings.Count(key, "%") == 1
}
