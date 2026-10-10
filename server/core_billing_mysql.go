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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func billingTimeMySQL(t time.Time) time.Time {
	if t.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return t.UTC().Truncate(time.Microsecond)
}

// Owner locks use the same order as native account deletion. Missing owners are
// allowed at this stage: an existing receipt may already belong to a tombstone.
// New rows still require a real owner through their foreign key.
func lockBillingOwnersMySQL(ctx context.Context, tx *sql.Tx, owners []uuid.UUID) error {
	sort.Slice(owners, func(i, j int) bool { return owners[i].String() < owners[j].String() })
	for i, id := range owners {
		if i > 0 && id == owners[i-1] {
			continue
		}
		var found string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", id).Scan(&found); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

func upsertPurchasesMySQL(ctx context.Context, db *sql.DB, purchases []*storagePurchase) ([]*storagePurchase, error) {
	if len(purchases) == 0 {
		return nil, errors.New("expects at least one receipt")
	}
	inputs := make([]storagePurchase, len(purchases))
	order := make([]int, len(purchases))
	owners := make([]uuid.UUID, len(purchases))
	seen := map[string]bool{}
	for i, p := range purchases {
		if p == nil || p.transactionId == "" {
			return nil, status.Error(codes.InvalidArgument, "A transaction ID is required.")
		}
		if seen[p.transactionId] {
			return nil, status.Error(codes.InvalidArgument, "Duplicate transaction in receipt batch.")
		}
		seen[p.transactionId] = true
		inputs[i] = *p
		order[i] = i
		owners[i] = p.userID
		inputs[i].refundTime = billingTimeMySQL(p.refundTime)
		inputs[i].purchaseTime = billingTimeMySQL(p.purchaseTime)
		if inputs[i].rawResponse == "" {
			inputs[i].rawResponse = "{}"
		}
		if !json.Valid([]byte(inputs[i].rawResponse)) {
			return nil, status.Error(codes.InvalidArgument, "Invalid provider response JSON.")
		}
	}
	sort.Slice(order, func(i, j int) bool { return inputs[order[i]].transactionId < inputs[order[j]].transactionId })
	var results []storagePurchase
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		results = append([]storagePurchase(nil), inputs...)
		if err := lockBillingOwnersMySQL(ctx, tx, append([]uuid.UUID(nil), owners...)); err != nil {
			return err
		}
		for _, i := range order {
			p := &results[i]
			var storedStore int
			err := tx.QueryRowContext(ctx, "SELECT store FROM purchase WHERE transaction_id = ? FOR UPDATE", p.transactionId).Scan(&storedStore)
			inserted := false
			if errors.Is(err, sql.ErrNoRows) {
				_, err = tx.ExecContext(ctx, "INSERT INTO purchase (user_id,store,transaction_id,product_id,purchase_time,raw_response,environment,refund_time) VALUES (?,?,?,?,?,?,?,?)", p.userID, p.store, p.transactionId, p.productId, p.purchaseTime, p.rawResponse, p.environment, p.refundTime)
				if mysqlErrorNumber(err, 1062) {
					err = tx.QueryRowContext(ctx, "SELECT store FROM purchase WHERE transaction_id = ? FOR UPDATE", p.transactionId).Scan(&storedStore)
				} else if err == nil {
					inserted = true
				}
			}
			if err != nil {
				return err
			}
			if !inserted {
				if storedStore != int(p.store) {
					return status.Error(codes.FailedPrecondition, "Transaction belongs to another store.")
				}
				// Revalidating a pre-refund receipt must not erase an already recorded refund.
				_, err = tx.ExecContext(ctx, "UPDATE purchase SET refund_time = IF(?, refund_time, ?), update_time = GREATEST(CURRENT_TIMESTAMP(6), update_time + INTERVAL 1 MICROSECOND) WHERE transaction_id = ?", p.refundTime.Unix() == 0, p.refundTime, p.transactionId)
				if err != nil {
					return err
				}
			}
			if err = tx.QueryRowContext(ctx, "SELECT user_id,store,product_id,purchase_time,raw_response,environment,refund_time,create_time,update_time FROM purchase WHERE transaction_id = ?", p.transactionId).Scan(&p.userID, &p.store, &p.productId, &p.purchaseTime, &p.rawResponse, &p.environment, &p.refundTime, &p.createTime, &p.updateTime); err != nil {
				return err
			}
			p.seenBefore = !inserted
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Some callback callers intentionally read the input pointers after success.
	for i := range purchases {
		*purchases[i] = results[i]
	}
	return purchases, nil
}

func upsertSubscriptionsMySQL(ctx context.Context, tx *sql.Tx, subs []storageSubscription) error {
	order := make([]int, len(subs))
	owners := make([]uuid.UUID, len(subs))
	for i := range subs {
		order[i] = i
		owners[i] = subs[i].userID
	}
	sort.SliceStable(order, func(i, j int) bool {
		return subs[order[i]].originalTransactionId < subs[order[j]].originalTransactionId
	})
	if err := lockBillingOwnersMySQL(ctx, tx, owners); err != nil {
		return err
	}
	for _, i := range order {
		s := &subs[i]
		if s.originalTransactionId == "" {
			return status.Error(codes.InvalidArgument, "An original transaction ID is required.")
		}
		s.refundTime = billingTimeMySQL(s.refundTime)
		s.purchaseTime = billingTimeMySQL(s.purchaseTime)
		s.expireTime = billingTimeMySQL(s.expireTime)
		// PostgreSQL uses to_jsonb(text) here: the persisted provider fields are JSON
		// strings, unlike purchase.raw_response. Preserve that existing API encoding.
		jsonText := func(v string) string {
			if v == "" {
				v = "{}"
			}
			b, _ := json.Marshal(v)
			return string(b)
		}
		rawResponse, rawNotification := s.rawResponse, s.rawNotification
		var storedStore int
		err := tx.QueryRowContext(ctx, "SELECT store FROM subscription WHERE original_transaction_id = ? FOR UPDATE", s.originalTransactionId).Scan(&storedStore)
		inserted := false
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, "INSERT INTO subscription (user_id,store,original_transaction_id,product_id,purchase_time,environment,expire_time,raw_response,raw_notification,refund_time) VALUES (?,?,?,?,?,?,?,?,?,?)", s.userID, s.store, s.originalTransactionId, s.productId, s.purchaseTime, s.environment, s.expireTime, jsonText(rawResponse), jsonText(rawNotification), s.refundTime)
			if mysqlErrorNumber(err, 1062) {
				err = tx.QueryRowContext(ctx, "SELECT store FROM subscription WHERE original_transaction_id = ? FOR UPDATE", s.originalTransactionId).Scan(&storedStore)
			} else if err == nil {
				inserted = true
			}
		}
		if err != nil {
			return err
		}
		if !inserted {
			if storedStore != int(s.store) {
				return status.Error(codes.FailedPrecondition, "Transaction belongs to another store.")
			}
			_, err = tx.ExecContext(ctx, "UPDATE subscription SET product_id = ?, expire_time = ?, raw_response = IF(? = '',raw_response,CAST(? AS JSON)), raw_notification = IF(? = '',raw_notification,CAST(? AS JSON)), refund_time = IF(?,refund_time,?), update_time = GREATEST(CURRENT_TIMESTAMP(6),update_time + INTERVAL 1 MICROSECOND) WHERE original_transaction_id = ?", s.productId, s.expireTime, rawResponse, jsonText(rawResponse), rawNotification, jsonText(rawNotification), s.refundTime.Unix() == 0, s.refundTime, s.originalTransactionId)
			if err != nil {
				return err
			}
		}
		if err = tx.QueryRowContext(ctx, "SELECT user_id,store,product_id,purchase_time,environment,expire_time,raw_response,raw_notification,refund_time,create_time,update_time FROM subscription WHERE original_transaction_id = ?", s.originalTransactionId).Scan(&s.userID, &s.store, &s.productId, &s.purchaseTime, &s.environment, &s.expireTime, &s.rawResponse, &s.rawNotification, &s.refundTime, &s.createTime, &s.updateTime); err != nil {
			return err
		}
	}
	return nil
}
