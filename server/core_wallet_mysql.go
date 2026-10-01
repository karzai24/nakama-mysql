// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/runtime"
)

func updateWalletsMySQL(ctx context.Context, db *sql.DB, updates []*walletUpdate, ledger bool) ([]*runtime.WalletUpdateResult, error) {
	var results []*runtime.WalletUpdateResult
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		var err error
		results, err = updateWalletsTxMySQL(ctx, tx, updates, ledger)
		return err
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// Lock each wallet once in canonical owner order. Repeated changesets still apply
// in input order, but every result owns its own before/after snapshot.
func updateWalletsTxMySQL(ctx context.Context, tx *sql.Tx, updates []*walletUpdate, ledger bool) ([]*runtime.WalletUpdateResult, error) {
	if len(updates) == 0 {
		return nil, nil
	}
	ids := map[string]bool{}
	for _, update := range updates {
		ids[update.UserID.String()] = true
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	wallets := map[string]map[string]int64{}
	for _, id := range ordered {
		var data string
		err := tx.QueryRowContext(ctx, "SELECT wallet FROM users WHERE id = ? FOR UPDATE", id).Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		wallet, err := decodeWallet(data)
		if err != nil {
			return nil, err
		}
		wallets[id] = wallet
	}
	results := make([]*runtime.WalletUpdateResult, 0, len(updates))
	type ledgerEntry struct {
		id                  uuid.UUID
		owner               string
		changeset, metadata string
	}
	entries := make([]ledgerEntry, 0, len(updates))
	for _, update := range updates {
		id := update.UserID.String()
		wallet, exists := wallets[id]
		if !exists {
			continue
		}
		result, err := applyWalletChanges(id, wallet, update.Changeset)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
		if ledger {
			changes, err := json.Marshal(update.Changeset)
			if err != nil {
				return nil, err
			}
			entries = append(entries, ledgerEntry{uuid.Must(uuid.NewV4()), id, string(changes), update.Metadata})
		}
	}
	for _, id := range ordered {
		wallet, exists := wallets[id]
		if !exists {
			continue
		}
		data, err := json.Marshal(wallet)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET wallet = ?,update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", string(data), id); err != nil {
			return nil, err
		}
	}
	for _, entry := range entries {
		if _, err := tx.ExecContext(ctx, "INSERT INTO wallet_ledger (id,user_id,changeset,metadata) VALUES (?,?,?,?)", entry.id, entry.owner, entry.changeset, entry.metadata); err != nil {
			return nil, err
		}
	}
	return results, nil
}

func updateWalletLedgerMySQL(ctx context.Context, db *sql.DB, id uuid.UUID, metadata string) (*walletLedger, error) {
	var result *walletLedger
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		result = nil
		// Establish the same user-before-ledger lock order used by wallet writes and
		// account deletion. Re-read the ledger under lock after finding its owner.
		var owner string
		if err := tx.QueryRowContext(ctx, "SELECT user_id FROM wallet_ledger WHERE id = ?", id).Scan(&owner); err != nil {
			return err
		}
		if err := lockStorageOwnersMySQL(ctx, tx, []string{owner}); err != nil {
			return err
		}
		var changes, old string
		var created time.Time
		if err := tx.QueryRowContext(ctx, "SELECT changeset,metadata,create_time FROM wallet_ledger WHERE id = ? AND user_id = ? FOR UPDATE", id, owner).Scan(&changes, &old, &created); err != nil {
			return err
		}
		merged, err := mergeFriendMetadataMySQL(old, metadata)
		if err != nil {
			return err
		}
		entry := &walletLedger{ID: id.String(), UserID: owner, CreateTime: created.Unix()}
		if err = json.Unmarshal([]byte(changes), &entry.Changeset); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(merged), &entry.Metadata); err != nil {
			return err
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if _, err = tx.ExecContext(ctx, "UPDATE wallet_ledger SET metadata = ?,update_time = ? WHERE id = ? AND user_id = ?", merged, now, id, owner); err != nil {
			return err
		}
		entry.UpdateTime = now.Unix()
		result = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func walletLedgerQueryMySQL(user uuid.UUID, limit *int, cursor *walletLedgerListCursor, after, before time.Time) (string, []any, error) {
	query := "SELECT id,changeset,metadata,create_time,update_time FROM wallet_ledger WHERE user_id = ?"
	args := []any{user}
	ascending := cursor != nil && !cursor.IsNext
	if cursor != nil {
		id, err := uuid.FromString(cursor.Id)
		if err != nil {
			return "", nil, runtime.ErrWalletLedgerInvalidCursor
		}
		op := "<"
		if ascending {
			op = ">"
		}
		query += " AND (create_time,id) " + op + " (?,?)"
		args = append(args, cursor.CreateTime, id)
	} else {
		query += " AND create_time < ?"
		args = append(args, time.Now().UTC())
	}
	if !after.IsZero() {
		query += " AND create_time > ?"
		args = append(args, after)
	}
	if !before.IsZero() {
		query += " AND create_time < ?"
		args = append(args, before)
	}
	if ascending {
		query += " ORDER BY create_time ASC,id ASC"
	} else {
		query += " ORDER BY create_time DESC,id DESC"
	}
	if limit != nil {
		query += " LIMIT ?"
		args = append(args, *limit+1)
	}
	return query, args, nil
}
