// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

func multiUpdateMySQL(ctx context.Context, db *sql.DB, metrics Metrics, accounts []*accountUpdate, writes StorageOpWrites, deletes StorageOpDeletes, index StorageIndex, wallets []*walletUpdate, ledger bool) ([]*api.StorageObjectAck, []*runtime.WalletUpdateResult, error) {
	// Lock the union first, rather than acquiring owners separately in each phase.
	// Missing account/wallet owners retain the upstream no-op behavior; a write to
	// missing storage owners is rejected by the storage helper inside this same tx.
	owners := map[string]bool{}
	for _, update := range accounts {
		owners[update.userID.String()] = true
	}
	for _, update := range wallets {
		owners[update.UserID.String()] = true
	}
	for _, op := range writes {
		id, err := uuid.FromString(op.OwnerID)
		if err != nil {
			return nil, nil, err
		}
		owners[id.String()] = true
	}
	for _, op := range deletes {
		id, err := uuid.FromString(op.OwnerID)
		if err != nil {
			return nil, nil, err
		}
		owners[id.String()] = true
	}
	ordered := make([]string, 0, len(owners))
	for id := range owners {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	var acks []*api.StorageObjectAck
	var results []*runtime.WalletUpdateResult
	var objects []*api.StorageObject
	var removed StorageOpDeletes
	var rejectedCollection string
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		acks, results, objects, removed = nil, nil, nil, nil
		rejectedCollection = ""
		for _, owner := range ordered {
			var id string
			if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", owner).Scan(&id); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if err := updateAccountsTxMySQL(ctx, tx, accounts); err != nil {
			return err
		}
		var err error
		objects = nil
		acks, objects, rejectedCollection, err = storageWriteTxMySQL(ctx, tx, true, writes)
		if err != nil {
			return err
		}
		removed, err = storageDeleteTxMySQL(ctx, tx, true, deletes)
		if err != nil {
			return err
		}
		results, err = updateWalletsTxMySQL(ctx, tx, wallets, ledger)
		return err
	})
	if err != nil {
		if errors.Is(err, runtime.ErrStorageRejectedVersion) && metrics != nil {
			metrics.StorageWriteRejectCount(map[string]string{"collection": rejectedCollection, "reason": "version"}, 1)
		}
		return nil, nil, err
	}
	publishMultiUpdateIndex(ctx, index, objects, removed)
	return acks, results, nil
}
