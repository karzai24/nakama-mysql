// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func billingPurchase(user uuid.UUID, id string) *storagePurchase {
	return &storagePurchase{userID: user, store: api.StoreProvider_APPLE_APP_STORE, transactionId: id, productId: "coins", purchaseTime: time.Now().UTC().Truncate(time.Microsecond), rawResponse: `{"large":9007199254740993}`, environment: api.StoreEnvironment_SANDBOX}
}
func billingSubscription(user uuid.UUID, id string) *storageSubscription {
	return &storageSubscription{userID: user, store: api.StoreProvider_APPLE_APP_STORE, originalTransactionId: id, productId: "monthly", purchaseTime: time.Now().UTC().Truncate(time.Microsecond), expireTime: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond), rawResponse: `{"verified":true}`, environment: api.StoreEnvironment_SANDBOX}
}

func TestDualBackendBillingPersistence(t *testing.T) {
	db := identityTestDB(t)
	ctx := context.Background()
	logger := zap.NewNop()
	owner, _ := lifecycleUser(t, db)
	id := "billing_" + GenerateString()
	subID := "billing_" + GenerateString()
	t.Cleanup(func() {
		lifecycleExec(t, db, "DELETE FROM purchase WHERE transaction_id = ?", id)
		lifecycleExec(t, db, "DELETE FROM subscription WHERE original_transaction_id = ?", subID)
	})
	p := billingPurchase(owner, id)
	out, err := upsertPurchases(ctx, db, []*storagePurchase{p})
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.False(t, out[0].seenBefore)
	require.False(t, out[0].createTime.IsZero())
	created := out[0].createTime
	retry := billingPurchase(owner, id)
	out, err = upsertPurchases(ctx, db, []*storagePurchase{retry})
	require.NoError(t, err)
	require.True(t, out[0].seenBefore)
	require.Equal(t, created, out[0].createTime)
	refunded := billingPurchase(owner, id)
	refunded.refundTime = time.Now().UTC().Truncate(time.Microsecond)
	_, err = upsertPurchases(ctx, db, []*storagePurchase{refunded})
	require.NoError(t, err)
	saved, err := GetPurchaseByTransactionId(ctx, logger, db, id)
	require.NoError(t, err)
	require.Equal(t, owner.String(), saved.UserId)
	require.Equal(t, refunded.refundTime.UTC(), saved.RefundTime.AsTime())
	require.JSONEq(t, p.rawResponse, saved.ProviderResponse)
	sub := billingSubscription(owner, subID)
	raw := sub.rawResponse
	require.NoError(t, persistSubscriptions(ctx, db, []*storageSubscription{sub}))
	require.False(t, sub.createTime.IsZero())
	encoded, _ := json.Marshal(raw)
	require.JSONEq(t, string(encoded), sub.rawResponse)
	existing, err := GetSubscriptionByProductId(ctx, logger, db, owner.String(), "monthly")
	require.NoError(t, err)
	require.True(t, existing.Active)
	created = sub.createTime
	renewal := billingSubscription(owner, subID)
	renewal.rawResponse = ""
	renewal.rawNotification = `{"renewed":true}`
	renewal.expireTime = renewal.expireTime.Add(time.Hour)
	require.NoError(t, persistSubscriptions(ctx, db, []*storageSubscription{renewal}))
	require.Equal(t, created, renewal.createTime)
	require.Equal(t, sub.rawResponse, renewal.rawResponse)
	refund := billingSubscription(owner, subID)
	refund.refundTime = time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, persistSubscriptions(ctx, db, []*storageSubscription{refund}))
	require.False(t, refund.Active())
	fresh := billingSubscription(owner, subID)
	fresh.rawResponse = ""
	fresh.rawNotification = ""
	require.NoError(t, persistSubscriptions(ctx, db, []*storageSubscription{fresh}))
	require.Equal(t, refund.refundTime.UTC(), fresh.refundTime.UTC())
	require.False(t, fresh.Active())
	stored, err := getSubscriptionByOriginalTransactionId(ctx, logger, db, subID)
	require.NoError(t, err)
	require.False(t, stored.Active)
	{
		stored, err = GetSubscriptionByProductId(ctx, logger, db, owner.String(), "monthly")
		require.NoError(t, err)
		require.False(t, stored.Active)
		_, err = upsertPurchases(ctx, db, []*storagePurchase{billingPurchase(owner, id)})
		require.NoError(t, err)
		saved, err = GetPurchaseByTransactionId(ctx, logger, db, id)
		require.NoError(t, err)
		require.Equal(t, refunded.refundTime.UTC(), saved.RefundTime.AsTime())
	}
}

func TestDualBackendBillingAtomicRollback(t *testing.T) {
	db := identityTestDB(t)
	ctx := context.Background()
	owner, _ := lifecycleUser(t, db)
	missing := uuid.Must(uuid.NewV4())
	suffix := GenerateString()
	pID := "a_" + suffix
	sID := "a_" + suffix
	t.Cleanup(func() {
		lifecycleExec(t, db, "DELETE FROM purchase WHERE transaction_id IN (?,?)", pID, "z_"+suffix)
		lifecycleExec(t, db, "DELETE FROM subscription WHERE original_transaction_id IN (?,?)", sID, "z_"+suffix)
	})
	p := billingPurchase(owner, pID)
	_, err := upsertPurchases(ctx, db, []*storagePurchase{p, billingPurchase(missing, "z_"+suffix)})
	require.Error(t, err)
	require.True(t, isBillingMissingUser(err))
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM purchase WHERE transaction_id = ?", pID))
	{
		require.True(t, p.createTime.IsZero())
	}
	sub := billingSubscription(owner, sID)
	original := *sub
	err = persistSubscriptions(ctx, db, []*storageSubscription{sub, billingSubscription(missing, "z_"+suffix)})
	require.Error(t, err)
	require.True(t, isBillingMissingUser(err))
	require.Equal(t, original, *sub)
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM subscription WHERE original_transaction_id = ?", sID))
	require.ErrorIs(t, persistSubscriptionNotification(ctx, db, billingSubscription(missing, "z_"+suffix)), ErrSkipNotification)
}

func TestDualBackendBillingOwnershipAndRaces(t *testing.T) {
	db := identityTestDB(t)
	ctx := context.Background()
	owner, _ := lifecycleUser(t, db)
	other, _ := lifecycleUser(t, db)
	id := "race_" + GenerateString()
	subID := "sub_" + GenerateString()
	t.Cleanup(func() {
		lifecycleExec(t, db, "DELETE FROM purchase WHERE transaction_id = ?", id)
		lifecycleExec(t, db, "DELETE FROM subscription WHERE original_transaction_id = ?", subID)
	})
	start := make(chan struct{})
	results := make(chan *storagePurchase, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uid := owner
			if i%2 == 1 {
				uid = other
			}
			p := billingPurchase(uid, id)
			<-start
			_, e := upsertPurchases(ctx, db, []*storagePurchase{p})
			results <- p
			errs <- e
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	saved, err := GetPurchaseByTransactionId(ctx, zap.NewNop(), db, id)
	require.NoError(t, err)
	newCount := 0
	for p := range results {
		require.Equal(t, saved.UserId, p.userID.String())
		if !p.seenBefore {
			newCount++
		}
	}
	require.Equal(t, 1, newCount)
	crossStore := billingPurchase(owner, id)
	crossStore.store = api.StoreProvider_GOOGLE_PLAY_STORE
	_, err = upsertPurchases(ctx, db, []*storagePurchase{crossStore})
	require.Error(t, err)
	// A deleted owner must remain the system owner, even when a new account revalidates.
	storedOwner := uuid.FromStringOrNil(saved.UserId)
	lifecycleExec(t, db, "DELETE FROM users WHERE id = ?", storedOwner)
	p := billingPurchase(other, id)
	_, err = upsertPurchases(ctx, db, []*storagePurchase{p})
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, p.userID)
	live, _ := lifecycleUser(t, db)
	sub := billingSubscription(live, subID)
	require.NoError(t, persistSubscriptions(ctx, db, []*storageSubscription{sub}))
	lifecycleExec(t, db, "DELETE FROM users WHERE id = ?", live)
	sub = billingSubscription(live, subID)
	require.NoError(t, persistSubscriptions(ctx, db, []*storageSubscription{sub}))
	require.Equal(t, uuid.Nil, sub.userID)
	// Same-receipt batch errors roll back instead of applying ambiguous updates.
	_, err = upsertPurchases(ctx, db, []*storagePurchase{billingPurchase(uuid.Nil, id), billingPurchase(uuid.Nil, id)})
	require.Error(t, err)
}

func TestDualBackendBillingRefundRace(t *testing.T) {
	db := identityTestDB(t)
	ctx := context.Background()
	owner, _ := lifecycleUser(t, db)
	id := "refund_" + GenerateString()
	t.Cleanup(func() { lifecycleExec(t, db, "DELETE FROM purchase WHERE transaction_id = ?", id) })
	_, err := upsertPurchases(ctx, db, []*storagePurchase{billingPurchase(owner, id)})
	require.NoError(t, err)
	errs := make(chan error, 8)
	refund := time.Now().UTC().Truncate(time.Microsecond)
	for i := 0; i < 8; i++ {
		go func(i int) {
			p := billingPurchase(owner, id)
			if i == 3 {
				p.refundTime = refund
			}
			_, e := upsertPurchases(ctx, db, []*storagePurchase{p})
			errs <- e
		}(i)
	}
	for i := 0; i < 8; i++ {
		require.NoError(t, <-errs, fmt.Sprint("worker", i))
	}
	p, err := GetPurchaseByTransactionId(ctx, zap.NewNop(), db, id)
	require.NoError(t, err)
	require.Equal(t, refund, p.RefundTime.AsTime())
}
