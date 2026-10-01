// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Provider HTTP is replaced with strict fixtures. HMAC/RSA checks and parsing run
// normally; this proves ingestion and DB behavior, not real store certification.
func TestDualBackendBillingStoreValidation(t *testing.T) {
	db := identityTestDB(t)
	ctx := context.Background()
	owner, _ := lifecycleUser(t, db)
	prefix := "stores_" + GenerateString()
	now := time.Now().UnixMilli()
	t.Cleanup(func() {
		lifecycleExec(t, db, "DELETE FROM purchase WHERE user_id = ?", owner)
		lifecycleExec(t, db, "DELETE FROM subscription WHERE user_id = ?", owner)
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	googleCfg := &IAPGoogleConfig{ClientEmail: prefix + "@local.invalid", PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))}
	huaweiCfg := &IAPHuaweiConfig{PublicKey: base64.StdEncoding.EncodeToString(pub), ClientID: prefix, ClientSecret: "local-fixture"}
	appleStatus, googleState := 0, 0
	old := httpc
	httpc = &http.Client{Transport: providerRoundTripper(func(r *http.Request) (*http.Response, error) {
		var body string
		switch {
		case r.URL.Host == "accounts.google.com" || r.URL.Host == "oauth-login.cloud.huawei.com":
			body = `{"access_token":"local-fixture","expires_in":3600}`
		case r.URL.Host == "buy.itunes.apple.com" && r.URL.Path == "/verifyReceipt":
			body = billingJSON(map[string]any{"status": appleStatus, "environment": "Sandbox", "receipt": map[string]any{"in_app": []any{map[string]string{"transaction_id": prefix + "apple", "product_id": "coins", "purchase_date_ms": strconv.FormatInt(now, 10)}}}, "latest_receipt_info": []any{map[string]string{"original_transaction_id": prefix + "sub", "product_id": "monthly", "original_purchase_date_ms": strconv.FormatInt(now, 10), "expires_date_ms": strconv.FormatInt(now+3600000, 10)}}})
		case r.URL.Host == "androidpublisher.googleapis.com" && strings.Contains(r.URL.Path, "/purchases/products/"):
			body = billingJSON(map[string]any{"purchaseState": googleState, "purchaseType": 0, "purchaseTimeMillis": strconv.FormatInt(now, 10)})
		case (r.URL.Host == "orders-dre.iap.hicloud.com" || r.URL.Host == "orders-at-dre.iap.dbankcloud.com") && r.URL.Path == "/applications/purchases/tokens/verify":
			body = billingJSON(map[string]any{"responseCode": "0", "purchaseTokenData": map[string]any{"purchaseToken": prefix + "huawei", "productId": "coins"}})
		case r.URL.Host == "iap.samsungapps.com" && r.URL.Path == "/iap/v6/receipt":
			body = billingJSON(map[string]any{"status": "success", "mode": "TEST", "packageName": "local.fixture", "itemId": "coins", "purchaseDate": time.Now().UTC().Format("2006-01-02 15:04:05")})
		default:
			return nil, fmt.Errorf("unexpected billing fixture route: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	t.Cleanup(func() { httpc = old })
	gReceipt := billingJSON(map[string]any{"packageName": "local.fixture", "productId": "coins", "purchaseToken": prefix + "google", "purchaseTime": now, "purchaseState": 0})
	hReceipt := billingJSON(map[string]any{"productId": "coins", "purchaseToken": prefix + "huawei", "purchaseTime": now, "purchaseType": 0})
	digest := sha256.Sum256([]byte(hReceipt))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	require.NoError(t, err)
	fbPayload := base64.RawURLEncoding.EncodeToString([]byte(billingJSON(map[string]any{"algorithm": "HMAC-SHA256", "product_id": "coins", "purchase_token": prefix + "facebook", "purchase_time": now / 1000})))
	mac := hmac.New(sha256.New, []byte("fixture-secret"))
	mac.Write([]byte(fbPayload))
	fbReceipt := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) + "." + fbPayload
	for _, test := range []struct {
		name     string
		validate func(bool) (*api.ValidatePurchaseResponse, error)
	}{
		{"apple", func(persist bool) (*api.ValidatePurchaseResponse, error) {
			return ValidatePurchasesApple(ctx, zap.NewNop(), db, owner, "fixture-password", "fixture-receipt", persist)
		}},
		{"google", func(persist bool) (*api.ValidatePurchaseResponse, error) {
			return ValidatePurchaseGoogle(ctx, zap.NewNop(), db, owner, googleCfg, gReceipt, persist)
		}},
		{"huawei", func(persist bool) (*api.ValidatePurchaseResponse, error) {
			return ValidatePurchaseHuawei(ctx, zap.NewNop(), db, owner, huaweiCfg, hReceipt, base64.StdEncoding.EncodeToString(signature), persist)
		}},
		{"facebook", func(persist bool) (*api.ValidatePurchaseResponse, error) {
			return ValidatePurchaseFacebookInstant(ctx, zap.NewNop(), db, owner, &IAPFacebookInstantConfig{AppSecret: "fixture-secret"}, fbReceipt, persist)
		}},
		{"samsung", func(persist bool) (*api.ValidatePurchaseResponse, error) {
			return ValidatePurchaseSamsung(ctx, zap.NewNop(), db, owner, &IAPSamsungConfig{PackageName: "local.fixture"}, prefix+"samsung", persist)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			v, e := test.validate(false)
			require.NoError(t, e)
			require.Len(t, v.ValidatedPurchases, 1)
			id := v.ValidatedPurchases[0].TransactionId
			require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM purchase WHERE transaction_id = ?", id))
			v, e = test.validate(true)
			require.NoError(t, e)
			require.Len(t, v.ValidatedPurchases, 1)
			require.False(t, v.ValidatedPurchases[0].SeenBefore)
			require.Equal(t, owner.String(), v.ValidatedPurchases[0].UserId)
			v, e = test.validate(true)
			require.NoError(t, e)
			require.True(t, v.ValidatedPurchases[0].SeenBefore)
		})
	}
	v, err := ValidateSubscriptionApple(ctx, zap.NewNop(), db, owner, "fixture-password", "fixture-receipt", true)
	require.NoError(t, err)
	require.True(t, v.ValidatedSubscription.Active)
	googleState = 1
	_, err = ValidatePurchaseGoogle(ctx, zap.NewNop(), db, owner, googleCfg, gReceipt, true)
	require.Error(t, err)
	appleStatus = 21002
	_, err = ValidatePurchasesApple(ctx, zap.NewNop(), db, owner, "fixture-password", "fixture-receipt", true)
	require.Error(t, err)
	_, err = ValidatePurchaseFacebookInstant(ctx, zap.NewNop(), db, owner, &IAPFacebookInstantConfig{AppSecret: "wrong-secret"}, fbReceipt, true)
	require.Error(t, err)
	_, err = ValidatePurchaseHuawei(ctx, zap.NewNop(), db, owner, huaweiCfg, hReceipt, "invalid-signature", true)
	require.Error(t, err)
}
