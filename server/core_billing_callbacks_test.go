// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func billingRequest(h http.HandlerFunc, body string) int {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return w.Code
}
func billingJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// Real Google OAuth signing and response parsing; all HTTP is local fixture data.
// No test contacts a store or uses a production key/account.
func TestDualBackendBillingGoogleCallbacks(t *testing.T) {
	db := identityTestDB(t)
	ctx := context.Background()
	owner, _ := lifecycleUser(t, db)
	id := "google_" + GenerateString()
	purchaseID := "product_" + GenerateString()
	t.Cleanup(func() {
		lifecycleExec(t, db, "DELETE FROM subscription WHERE original_transaction_id = ?", id)
		lifecycleExec(t, db, "DELETE FROM purchase WHERE transaction_id = ?", purchaseID)
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	cfg := &IAPGoogleConfig{ClientEmail: GenerateString() + "@local.invalid", PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))}
	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	subPayload := map[string]any{"lineItems": []any{map[string]any{"productId": "monthly", "expiryTime": expiry}}, "externalAccountIdentifiers": map[string]string{"obfuscatedExternalAccountId": owner.String()}, "testPurchase": map[string]any{}}
	productPayload := map[string]any{"productLineItem": []any{map[string]any{"productId": "coins"}}, "purchaseCompletionTime": time.Now().UTC(), "obfuscatedExternalAccountId": owner.String()}
	responseCode := http.StatusOK
	old := httpc
	httpc = &http.Client{Transport: providerRoundTripper(func(r *http.Request) (*http.Response, error) {
		var body string
		code := responseCode
		switch {
		case r.URL.Host == "accounts.google.com" && r.URL.Path == "/o/oauth2/token":
			body = `{"access_token":"local-fixture","expires_in":3600}`
			code = 200
		case r.URL.Host == "androidpublisher.googleapis.com" && strings.Contains(r.URL.Path, "/subscriptionsv2/tokens/"):
			body = billingJSON(subPayload)
		case r.URL.Host == "androidpublisher.googleapis.com" && strings.Contains(r.URL.Path, "/productsv2/tokens/"):
			body = billingJSON(productPayload)
		default:
			return nil, fmt.Errorf("unexpected fixture request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	t.Cleanup(func() { httpc = old })
	subCalls, purchaseCalls := 0, 0
	failHook := false
	h := googleNotificationHandler(zap.NewNop(), db, cfg, func(ctx context.Context, n runtime.NotificationType, p *api.ValidatedPurchase, _ *runtime.PurchaseV2GoogleResponse) error {
		purchaseCalls++
		saved, e := GetPurchaseByTransactionId(ctx, zap.NewNop(), db, p.TransactionId)
		require.NoError(t, e)
		require.Equal(t, p.RefundTime.AsTime(), saved.RefundTime.AsTime())
		require.False(t, p.CreateTime.AsTime().IsZero())
		if failHook {
			return errors.New("fixture hook error")
		}
		return nil
	}, func(ctx context.Context, n runtime.NotificationType, s *api.ValidatedSubscription, _ *runtime.SubscriptionV2GoogleResponse) error {
		subCalls++
		saved, e := getSubscriptionByOriginalTransactionId(ctx, zap.NewNop(), db, s.OriginalTransactionId)
		require.NoError(t, e)
		require.NotNil(t, saved)
		require.Equal(t, s.UserId, saved.UserId)
		require.Equal(t, s.Active, saved.Active)
		require.False(t, s.CreateTime.AsTime().IsZero())
		if n == runtime.IAPNotificationRefunded {
			require.False(t, s.Active)
			require.Greater(t, s.RefundTime.Seconds, int64(0))
		}
		return nil
	})
	notify := func(kind string, data any) string {
		return billingJSON(map[string]any{"message": map[string]string{"data": base64.StdEncoding.EncodeToString([]byte(billingJSON(map[string]any{"packageName": "local.fixture", kind: data})))}})
	}
	renewed := notify("subscriptionNotification", map[string]any{"notificationType": runtime.GoogleSubscriptionRenewed, "purchaseToken": id})
	require.Equal(t, 200, billingRequest(h, renewed))
	require.Equal(t, 1, subCalls)
	refunded := notify("voidedPurchaseNotification", map[string]any{"productType": runtime.GoogleProductTypeSubscription, "purchaseToken": id})
	require.Equal(t, 200, billingRequest(h, refunded))
	require.Equal(t, 2, subCalls)
	// A duplicate callback is delivered again: receipt idempotency is not hook exactly-once delivery.
	require.Equal(t, 200, billingRequest(h, refunded))
	require.Equal(t, 3, subCalls)
	subPayload["lineItems"] = []any{}
	require.Equal(t, 500, billingRequest(h, renewed))
	require.Equal(t, 3, subCalls)
	subPayload["lineItems"] = []any{map[string]any{"productId": "monthly", "expiryTime": expiry}}
	subPayload["externalAccountIdentifiers"] = map[string]string{"obfuscatedExternalAccountId": uuid.Must(uuid.NewV4()).String()}
	missing := notify("subscriptionNotification", map[string]any{"notificationType": runtime.GoogleSubscriptionRenewed, "purchaseToken": "missing_" + id})
	require.Equal(t, 200, billingRequest(h, missing))
	require.Equal(t, 3, subCalls)
	responseCode = 503
	require.Equal(t, 500, billingRequest(h, renewed))
	require.Equal(t, 3, subCalls)
	responseCode = 200
	productRefund := notify("voidedPurchaseNotification", map[string]any{"productType": 2, "purchaseToken": purchaseID})
	failHook = true
	require.Equal(t, 500, billingRequest(h, productRefund))
	require.Equal(t, 1, purchaseCalls)
	failHook = false
	require.Equal(t, 200, billingRequest(h, productRefund))
	require.Equal(t, 2, purchaseCalls)
	p, e := GetPurchaseByTransactionId(ctx, zap.NewNop(), db, purchaseID)
	require.NoError(t, e)
	require.Greater(t, p.RefundTime.Seconds, int64(0))
	for _, body := range []string{"null", `{"message":{"data":"%%%"}}`, `{"message":{"data":"bnVsbA=="}}`} {
		require.Equal(t, 500, billingRequest(h, body))
	}
	// Validation's persist=false path must perform no write.
	subPayload["externalAccountIdentifiers"] = map[string]string{"obfuscatedExternalAccountId": owner.String()}
	receipt := billingJSON(map[string]any{"packageName": "local.fixture", "productId": "monthly", "purchaseToken": "unpersisted_" + id, "purchaseTime": time.Now().UnixMilli()})
	_, e = ValidateSubscriptionGoogle(ctx, zap.NewNop(), db, owner, cfg, receipt, false)
	require.NoError(t, e)
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM subscription WHERE original_transaction_id = ?", "unpersisted_"+id))
}

// Tests the post-verification persistence boundary, not Apple's signing service.
// The production entry point is also checked to reject invalid signed payloads.
func TestDualBackendBillingAppleCallbacks(t *testing.T) {
	db := identityTestDB(t)
	owner, _ := lifecycleUser(t, db)
	id := "apple_" + GenerateString()
	t.Cleanup(func() { lifecycleExec(t, db, "DELETE FROM subscription WHERE original_transaction_id = ?", id) })
	payload := &appleNotificationPayload{NotificationType: "SUBSCRIBED"}
	data := &runtime.AppleNotificationData{TransactionInfo: &runtime.AppleNotificationTransactionInfo{AppAccountToken: owner.String(), OriginalTransactionId: id, ProductId: "monthly", OriginalPurchaseDate: time.Now().UnixMilli(), ExpiresDate: time.Now().Add(time.Hour).UnixMilli(), Environment: "Sandbox"}}
	decode := func(*appleNotificationSigned) (*appleNotificationPayload, *runtime.AppleNotificationData, error) {
		return payload, data, nil
	}
	calls := 0
	hook := func(ctx context.Context, n runtime.NotificationType, s *api.ValidatedSubscription, _ *runtime.AppleNotificationData) error {
		calls++
		saved, e := getSubscriptionByOriginalTransactionId(ctx, zap.NewNop(), db, id)
		require.NoError(t, e)
		require.NotNil(t, saved)
		require.Equal(t, s.UserId, saved.UserId)
		require.Equal(t, s.Active, saved.Active)
		require.Equal(t, s.CreateTime.AsTime(), saved.CreateTime.AsTime())
		if n == runtime.IAPNotificationRefunded {
			require.False(t, s.Active)
			require.Greater(t, s.RefundTime.Seconds, int64(0))
		}
		return nil
	}
	h := appleNotificationHandlerWithDecoder(zap.NewNop(), db, nil, hook, decode)
	body := `{"signedPayload":"test-boundary-only"}`
	require.Equal(t, 200, billingRequest(h, body))
	require.Equal(t, 1, calls)
	payload.NotificationType = "REFUND"
	data.TransactionInfo.RevocationDate = time.Now().UnixMilli()
	require.Equal(t, 200, billingRequest(h, body))
	require.Equal(t, 2, calls)
	require.Equal(t, 200, billingRequest(appleNotificationHandlerWithDecoder(zap.NewNop(), db, nil, nil, decode), body))
	data.TransactionInfo = nil
	require.Equal(t, 500, billingRequest(h, body))
	require.Equal(t, 2, calls)
	real := appleNotificationHandler(zap.NewNop(), db, nil, hook)
	require.Equal(t, 500, billingRequest(real, body))
	require.Equal(t, 500, billingRequest(real, "null"))
	require.Equal(t, 2, calls)
}
