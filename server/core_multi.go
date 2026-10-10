// Copyright 2020 The Nakama Authors
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
	"context"
	"database/sql"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"

	"go.uber.org/zap"
)

func MultiUpdate(ctx context.Context, logger *zap.Logger, db *sql.DB, metrics Metrics, accountUpdates []*accountUpdate, storageWrites StorageOpWrites, storageDeletes StorageOpDeletes, storageIndex StorageIndex, walletUpdates []*walletUpdate, updateLedger bool) ([]*api.StorageObjectAck, []*runtime.WalletUpdateResult, error) {
	if len(accountUpdates) == 0 && len(storageWrites) == 0 && len(storageDeletes) == 0 && len(walletUpdates) == 0 {
		return nil, nil, nil
	}

	return multiUpdateMySQL(ctx, db, metrics, accountUpdates, storageWrites, storageDeletes, storageIndex, walletUpdates, updateLedger)

}

func publishMultiUpdateIndex(ctx context.Context, index StorageIndex, objects []*api.StorageObject, removed StorageOpDeletes) {
	// Publish only the final committed state. A write followed by a delete of the
	// same object must not briefly reinsert that object into an index after commit.
	deleted := map[[3]string]bool{}
	for _, op := range removed {
		deleted[[3]string{op.OwnerID, op.ObjectID.Collection, op.ObjectID.Key}] = true
	}
	final := make([]*api.StorageObject, 0, len(objects))
	positions := map[[3]string]int{}
	for _, object := range objects {
		key := [3]string{object.UserId, object.Collection, object.Key}
		if deleted[key] {
			continue
		}
		if position, exists := positions[key]; exists {
			final[position] = object
		} else {
			positions[key] = len(final)
			final = append(final, object)
		}
	}
	if len(final) > 0 {
		index.Write(ctx, final)
	}
	if len(removed) > 0 {
		index.Delete(ctx, removed)
	}
}
