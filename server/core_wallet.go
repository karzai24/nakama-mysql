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
	"fmt"
	"slices"

	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/runtime"

	"go.uber.org/zap"
)

type walletLedgerListCursor struct {
	UserId     string
	CreateTime time.Time
	Id         string
	IsNext     bool
	After      time.Time
	Before     time.Time
}

// Not an API entity, only used to receive data from runtime environment.
type walletUpdate struct {
	UserID    uuid.UUID
	Changeset map[string]int64
	// Metadata is expected to be a valid JSON string already.
	Metadata string
}

// Not an API entity, only used to send data to runtime environment.
type walletLedger struct {
	ID         string
	UserID     string
	Changeset  map[string]int64
	Metadata   map[string]any
	CreateTime int64
	UpdateTime int64
}

func (w *walletLedger) GetID() string {
	return w.ID
}

func (w *walletLedger) GetUserID() string {
	return w.UserID
}

func (w *walletLedger) GetCreateTime() int64 {
	return w.CreateTime
}

func (w *walletLedger) GetUpdateTime() int64 {
	return w.UpdateTime
}

func (w *walletLedger) GetChangeset() map[string]int64 {
	return w.Changeset
}

func (w *walletLedger) GetMetadata() map[string]any {
	return w.Metadata
}

func UpdateWallets(ctx context.Context, logger *zap.Logger, db *sql.DB, updates []*walletUpdate, updateLedger bool) ([]*runtime.WalletUpdateResult, error) {
	if len(updates) == 0 {
		return nil, nil
	}

	return updateWalletsMySQL(ctx, db, updates, updateLedger)

}

func UpdateWalletLedger(ctx context.Context, logger *zap.Logger, db *sql.DB, id uuid.UUID, metadata string) (*walletLedger, error) {

	return updateWalletLedgerMySQL(ctx, db, id, metadata)

}

func ListWalletLedger(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, limit *int, cursor string, after, before time.Time) ([]*walletLedger, string, string, error) {
	var incomingCursor *walletLedgerListCursor
	if cursor != "" {
		cb, err := base64.URLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", "", runtime.ErrWalletLedgerInvalidCursor
		}
		incomingCursor = &walletLedgerListCursor{}
		if err := gob.NewDecoder(bytes.NewReader(cb)).Decode(incomingCursor); err != nil {
			return nil, "", "", runtime.ErrWalletLedgerInvalidCursor
		}

		if _, err := uuid.FromString(incomingCursor.Id); err != nil {
			return nil, "", "", runtime.ErrWalletLedgerInvalidCursor
		}

		// Cursor and filter mismatch. Perhaps the caller has sent an old cursor with a changed filter.
		if userID.String() != incomingCursor.UserId {
			return nil, "", "", runtime.ErrWalletLedgerInvalidCursor
		}
		if !after.Equal(incomingCursor.After) {
			return nil, "", "", runtime.ErrWalletLedgerInvalidCursor
		}
		if !before.Equal(incomingCursor.Before) {
			return nil, "", "", runtime.ErrWalletLedgerInvalidCursor
		}
	}

	if limit != nil && *limit == 0 {
		return []*walletLedger{}, "", "", nil
	}
	params := []interface{}{userID, time.Now().UTC(), uuid.UUID{}}
	if incomingCursor != nil {
		params[1] = incomingCursor.CreateTime
		params[2] = incomingCursor.Id
	}

	query := ""
	order := " ORDER BY create_time DESC, id DESC"
	if incomingCursor != nil && !incomingCursor.IsNext {

		order = " ORDER BY create_time ASC, id ASC"
	}
	if !after.IsZero() {
		params = append(params, after)
		query += fmt.Sprintf(" AND create_time > $%v", len(params))
	}
	if !before.IsZero() {
		params = append(params, before)
		query += fmt.Sprintf(" AND create_time < $%v", len(params))
	}
	query += order
	if limit != nil {
		query += fmt.Sprintf(` LIMIT %v`, *limit+1)
	}

	{
		var err error
		query, params, err = walletLedgerQueryMySQL(userID, limit, incomingCursor, after, before)
		if err != nil {
			return nil, "", "", err
		}
	}
	results := make([]*walletLedger, 0, 10)
	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		logger.Error("Error retrieving user wallet ledger.", zap.String("user_id", userID.String()), zap.Error(err))
		return nil, "", "", err
	}
	defer rows.Close()

	var id string
	var changeset sql.NullString
	var metadata sql.NullString
	var createTime sql.NullTime
	var updateTime sql.NullTime
	var nextCursor *walletLedgerListCursor
	var prevCursor *walletLedgerListCursor
	for rows.Next() {
		if limit != nil && len(results) >= *limit {
			nextCursor = &walletLedgerListCursor{
				UserId:     userID.String(),
				Id:         id,
				CreateTime: createTime.Time,
				IsNext:     true,
			}
			break
		}

		err = rows.Scan(&id, &changeset, &metadata, &createTime, &updateTime)
		if err != nil {
			logger.Error("Error converting user wallet ledger.", zap.String("user_id", userID.String()), zap.Error(err))
			return nil, "", "", err
		}

		var changesetMap map[string]int64
		err = json.Unmarshal([]byte(changeset.String), &changesetMap)
		if err != nil {
			logger.Error("Error converting user wallet ledger changeset.", zap.String("user_id", userID.String()), zap.Error(err))
			return nil, "", "", err
		}

		var metadataMap map[string]any
		err = json.Unmarshal([]byte(metadata.String), &metadataMap)
		if err != nil {
			logger.Error("Error converting user wallet ledger metadata.", zap.String("user_id", userID.String()), zap.Error(err))
			return nil, "", "", err
		}

		results = append(results, &walletLedger{
			ID:         id,
			UserID:     userID.String(),
			Changeset:  changesetMap,
			Metadata:   metadataMap,
			CreateTime: createTime.Time.Unix(),
			UpdateTime: updateTime.Time.Unix(),
		})

		if incomingCursor != nil && prevCursor == nil {
			prevCursor = &walletLedgerListCursor{
				UserId:     userID.String(),
				Id:         id,
				CreateTime: createTime.Time,
				IsNext:     false,
			}
		}
	}

	if incomingCursor != nil && !incomingCursor.IsNext {
		if nextCursor != nil && prevCursor != nil {
			nextCursor, nextCursor.IsNext, prevCursor, prevCursor.IsNext = prevCursor, prevCursor.IsNext, nextCursor, nextCursor.IsNext
		} else if nextCursor != nil {
			nextCursor, prevCursor = nil, nextCursor
			prevCursor.IsNext = !prevCursor.IsNext
		} else if prevCursor != nil {
			nextCursor, prevCursor = prevCursor, nil
			nextCursor.IsNext = !nextCursor.IsNext
		}

		slices.Reverse(results)
	}

	if err := rows.Err(); err != nil {
		return nil, "", "", err
	}
	for _, out := range []*walletLedgerListCursor{nextCursor, prevCursor} {
		if out != nil {
			out.After, out.Before = after, before
		}
	}
	var nextCursorStr string
	if nextCursor != nil {
		cursorBuf := new(bytes.Buffer)
		if err := gob.NewEncoder(cursorBuf).Encode(nextCursor); err != nil {
			logger.Error("Error creating wallet ledger list cursor", zap.Error(err))
			return nil, "", "", err
		}
		nextCursorStr = base64.URLEncoding.EncodeToString(cursorBuf.Bytes())
	}

	var prevCursorStr string
	if prevCursor != nil {
		cursorBuf := new(bytes.Buffer)
		if err := gob.NewEncoder(cursorBuf).Encode(prevCursor); err != nil {
			logger.Error("Error creating wallet ledger list cursor", zap.Error(err))
			return nil, "", "", err
		}
		prevCursorStr = base64.URLEncoding.EncodeToString(cursorBuf.Bytes())
	}

	return results, nextCursorStr, prevCursorStr, nil
}
