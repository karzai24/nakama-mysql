// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid/v5"
)

// Publish returned receipt state only after the transaction commits. A retry must
// start from the original provider data, not a previous attempt's encoded result.
func persistSubscriptions(ctx context.Context, db *sql.DB, subscriptions []*storageSubscription) error {
	if len(subscriptions) == 0 {
		return errors.New("expects at least one subscription")
	}
	inputs := make([]storageSubscription, len(subscriptions))
	for i, s := range subscriptions {
		if s == nil {
			return errors.New("subscription is nil")
		}
		inputs[i] = *s
	}
	var results []storageSubscription
	err := ExecuteInTx(ctx, db, func(tx *sql.Tx) error {
		results = append([]storageSubscription(nil), inputs...)
		{
			return upsertSubscriptionsMySQL(ctx, tx, results)
		}

	})
	if err != nil {
		return err
	}
	for i := range subscriptions {
		*subscriptions[i] = results[i]
	}
	return nil
}

func isBillingMissingUser(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == 1452 && (strings.Contains(myErr.Message, "purchase_user_id_fkey") || strings.Contains(myErr.Message, "subscription_user_id_fkey"))
}
func persistSubscriptionNotification(ctx context.Context, db *sql.DB, sub *storageSubscription) error {
	err := persistSubscriptions(ctx, db, []*storageSubscription{sub})
	if isBillingMissingUser(err) {
		return ErrSkipNotification
	}
	return err
}

func billingUserID(id uuid.UUID) string {
	if id.IsNil() {
		return ""
	}
	return id.String()
}
