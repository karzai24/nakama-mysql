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
	"context"
	"database/sql"
	"encoding/base64"

	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"strconv"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/iap"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var ErrPurchasesListInvalidCursor = errors.New("purchases list cursor invalid")

var httpc = &http.Client{Timeout: 20 * time.Second}

func ValidatePurchasesApple(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, password, receipt string, persist bool) (*api.ValidatePurchaseResponse, error) {
	tokens := strings.Split(receipt, ".")
	if len(tokens) != 3 {
		// Receipt is not a JWS, fallback to using deprecated verifyReceipt API.
		return validateLegacyPurchaseReceiptApple(ctx, logger, db, httpc, userID, receipt, password, persist)
	}
	// Receipt is a JWS.
	if err := iap.ValidateAppleJwsSignature(receipt); err != nil {
		return nil, err
	}
	seg := tokens[1]
	jsonPayload, err := base64.RawStdEncoding.DecodeString(seg)
	if err != nil {
		return nil, err
	}

	var transactionInfo *runtime.AppleNotificationTransactionInfo
	if err = json.Unmarshal(jsonPayload, &transactionInfo); err != nil {
		return nil, err
	}

	if transactionInfo.ExpiresDate != 0 {
		// This is a subscription transaction.
		return nil, status.Error(codes.FailedPrecondition, "Subscription Receipt. Use the appropriate function instead.")
	}

	env := api.StoreEnvironment_PRODUCTION
	if transactionInfo.Environment == iap.AppleSandboxEnvironment {
		env = api.StoreEnvironment_SANDBOX
	}

	dbPurchase := &storagePurchase{
		userID:        userID,
		store:         api.StoreProvider_APPLE_APP_STORE,
		productId:     transactionInfo.ProductId,
		transactionId: transactionInfo.TransactionId,
		purchaseTime:  parseMillisecondUnixTimestamp(transactionInfo.PurchaseDate),
		refundTime:    parseMillisecondUnixTimestamp(transactionInfo.RevocationDate),
		environment:   env,
	}

	validatedPurchase := &api.ValidatedPurchase{
		UserId:        dbPurchase.userID.String(),
		ProductId:     transactionInfo.ProductId,
		TransactionId: transactionInfo.TransactionId,
		Store:         api.StoreProvider_APPLE_APP_STORE,
		PurchaseTime:  timestamppb.New(dbPurchase.purchaseTime),
		RefundTime:    timestamppb.New(dbPurchase.refundTime),
		Environment:   env,
	}

	if !persist {
		return &api.ValidatePurchaseResponse{ValidatedPurchases: []*api.ValidatedPurchase{validatedPurchase}}, nil
	}

	receiptJson, err := json.Marshal(map[string]string{"jws": receipt})
	if err != nil {
		logger.Error("Failed to marshal Apple receipt JSON", zap.Error(err))
		return nil, status.Error(codes.Internal, "Error validating Apple JWS receipt")
	}
	dbPurchase.rawResponse = string(receiptJson)

	dbPurchases, err := upsertPurchases(ctx, db, []*storagePurchase{dbPurchase})
	if err != nil || len(dbPurchases) == 0 {
		logger.Error("Failed to store App Store notification purchase data", zap.Error(err))
		return nil, status.Error(codes.Internal, "Failed to store app store purchase data")
	}

	dbPurchase = dbPurchases[0]
	suid := dbPurchase.userID.String()
	if dbPurchase.userID.IsNil() {
		suid = ""
	}

	validatedPurchase.UserId = suid
	validatedPurchase.SeenBefore = dbPurchase.seenBefore
	validatedPurchase.CreateTime = timestamppb.New(dbPurchase.createTime)
	validatedPurchase.UpdateTime = timestamppb.New(dbPurchase.updateTime)
	validatedPurchase.RefundTime = timestamppb.New(dbPurchase.refundTime)
	validatedPurchase.ProviderResponse = "" // Do not set for jws validation; no point in returning the incoming receipt.

	return &api.ValidatePurchaseResponse{ValidatedPurchases: []*api.ValidatedPurchase{validatedPurchase}}, nil
}

func validateLegacyPurchaseReceiptApple(ctx context.Context, logger *zap.Logger, db *sql.DB, httpc *http.Client, userID uuid.UUID, receipt, password string, persist bool) (*api.ValidatePurchaseResponse, error) {
	if password == "" {
		return nil, status.Error(codes.FailedPrecondition, "Apple IAP is not configured.")
	}

	validation, raw, err := iap.ValidateLegacyReceiptApple(ctx, httpc, password, receipt)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			if vErr, ok := errors.AsType[*iap.ValidationError](err); ok {
				logger.Debug("Error validating Apple receipt", zap.Error(vErr.Err), zap.Int("status_code", vErr.StatusCode), zap.String("payload", vErr.Payload))
				return nil, vErr
			}
			logger.Error("Error validating Apple receipt", zap.Error(err))
		}
		return nil, err
	}

	if validation.Status != iap.AppleReceiptIsValid {
		if validation.IsRetryable {
			return nil, status.Error(codes.Unavailable, "Apple IAP verification is currently unavailable. Try again later.")
		}
		return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("Invalid Receipt. Status: %d", validation.Status))
	}

	env := api.StoreEnvironment_PRODUCTION
	if validation.Environment == iap.AppleSandboxEnvironment {
		env = api.StoreEnvironment_SANDBOX
	}

	seenTransactionIDs := make(map[string]struct{}, len(validation.Receipt.InApp)+len(validation.LatestReceiptInfo))
	storagePurchases := make([]*storagePurchase, 0, len(validation.Receipt.InApp)+len(validation.LatestReceiptInfo))
	for _, purchase := range validation.Receipt.InApp {
		if purchase.ExpiresDateMs != "" {
			continue
		}
		if _, seen := seenTransactionIDs[purchase.TransactionId]; seen {
			continue
		}

		purchaseTime, err := strconv.ParseInt(purchase.PurchaseDateMs, 10, 64)
		if err != nil {
			return nil, err
		}

		seenTransactionIDs[purchase.TransactionId] = struct{}{}
		storagePurchases = append(storagePurchases, &storagePurchase{
			userID:        userID,
			store:         api.StoreProvider_APPLE_APP_STORE,
			productId:     purchase.ProductID,
			transactionId: purchase.TransactionId,
			rawResponse:   string(raw),
			purchaseTime:  parseMillisecondUnixTimestamp(purchaseTime),
			environment:   env,
		})
	}
	// latest_receipt_info can also contaion purchases.
	// https://developer.apple.com/forums/thread/63092
	for _, purchase := range validation.LatestReceiptInfo {
		if purchase.ExpiresDateMs != "" {
			continue
		}
		if _, seen := seenTransactionIDs[purchase.TransactionId]; seen {
			continue
		}

		purchaseTime, err := strconv.ParseInt(purchase.PurchaseDateMs, 10, 64)
		if err != nil {
			return nil, err
		}

		seenTransactionIDs[purchase.TransactionId] = struct{}{}
		storagePurchases = append(storagePurchases, &storagePurchase{
			userID:        userID,
			store:         api.StoreProvider_APPLE_APP_STORE,
			productId:     purchase.ProductId,
			transactionId: purchase.TransactionId,
			rawResponse:   string(raw),
			purchaseTime:  parseMillisecondUnixTimestamp(purchaseTime),
			environment:   env,
		})
	}

	if len(storagePurchases) == 0 && len(validation.Receipt.InApp)+len(validation.LatestReceiptInfo) > 0 {
		// All purchases in this receipt are subscriptions.
		return nil, status.Error(codes.FailedPrecondition, "Subscription Receipt. Use the appropriate function instead.")
	}

	if !persist {
		// Skip storing the receipts
		validatedPurchases := make([]*api.ValidatedPurchase, 0, len(storagePurchases))
		for _, p := range storagePurchases {
			validatedPurchases = append(validatedPurchases, &api.ValidatedPurchase{
				UserId:           p.userID.String(),
				ProductId:        p.productId,
				TransactionId:    p.transactionId,
				Store:            p.store,
				PurchaseTime:     timestamppb.New(p.purchaseTime),
				ProviderResponse: string(raw),
				Environment:      p.environment,
			})
		}

		return &api.ValidatePurchaseResponse{ValidatedPurchases: validatedPurchases}, nil
	}

	purchases, err := upsertPurchases(ctx, db, storagePurchases)
	if err != nil {
		return nil, err
	}

	validatedPurchases := make([]*api.ValidatedPurchase, 0, len(purchases))
	for _, p := range purchases {
		suid := p.userID.String()
		if p.userID.IsNil() {
			suid = ""
		}
		validatedPurchases = append(validatedPurchases, &api.ValidatedPurchase{
			UserId:           suid,
			ProductId:        p.productId,
			TransactionId:    p.transactionId,
			Store:            p.store,
			PurchaseTime:     timestamppb.New(p.purchaseTime),
			CreateTime:       timestamppb.New(p.createTime),
			UpdateTime:       timestamppb.New(p.updateTime),
			ProviderResponse: string(raw),
			SeenBefore:       p.seenBefore,
			Environment:      p.environment,
		})
	}

	return &api.ValidatePurchaseResponse{
		ValidatedPurchases: validatedPurchases,
	}, nil
}

func ValidatePurchaseGoogle(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, config *IAPGoogleConfig, receipt string, persist bool) (*api.ValidatePurchaseResponse, error) {
	gResponse, gReceipt, raw, err := iap.ValidateReceiptGoogle(ctx, httpc, config.ClientEmail, config.PrivateKey, receipt)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			if vErr, ok := errors.AsType[*iap.ValidationError](err); ok {
				logger.Debug("Error validating Google receipt", zap.Error(vErr.Err), zap.Int("status_code", vErr.StatusCode), zap.String("payload", vErr.Payload))
				return nil, vErr
			} else {
				logger.Error("Error validating Google receipt", zap.Error(err))
			}
		}
		return nil, err
	}

	purchaseEnv := api.StoreEnvironment_PRODUCTION
	if gResponse.PurchaseType == 0 {
		purchaseEnv = api.StoreEnvironment_SANDBOX
	}

	if gResponse.PurchaseState != 0 || gReceipt.PurchaseState != 0 {
		// The provider response is authoritative; never accept a client-claimed
		// purchased state when Google reports cancelled or pending.
		return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("Invalid Receipt. State: %d", gReceipt.PurchaseState))
	}

	sPurchase := &storagePurchase{
		userID:        userID,
		store:         api.StoreProvider_GOOGLE_PLAY_STORE,
		productId:     gReceipt.ProductID,
		transactionId: gReceipt.PurchaseToken,
		rawResponse:   string(raw),
		purchaseTime:  parseMillisecondUnixTimestamp(gReceipt.PurchaseTime),
		environment:   purchaseEnv,
	}

	if !persist {
		validatedPurchases := []*api.ValidatedPurchase{
			{
				UserId:           userID.String(),
				ProductId:        sPurchase.productId,
				TransactionId:    sPurchase.transactionId,
				Store:            sPurchase.store,
				PurchaseTime:     timestamppb.New(sPurchase.purchaseTime),
				ProviderResponse: string(raw),
				Environment:      sPurchase.environment,
			},
		}

		return &api.ValidatePurchaseResponse{ValidatedPurchases: validatedPurchases}, nil
	}

	purchases, err := upsertPurchases(ctx, db, []*storagePurchase{sPurchase})
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("Error storing Google receipt", zap.Error(err))
		}
		return nil, err
	}

	validatedPurchases := make([]*api.ValidatedPurchase, 0, len(purchases))
	for _, p := range purchases {
		suid := p.userID.String()
		if p.userID.IsNil() {
			suid = ""
		}
		validatedPurchases = append(validatedPurchases, &api.ValidatedPurchase{
			UserId:           suid,
			ProductId:        p.productId,
			TransactionId:    p.transactionId,
			Store:            p.store,
			PurchaseTime:     timestamppb.New(p.purchaseTime),
			CreateTime:       timestamppb.New(p.createTime),
			UpdateTime:       timestamppb.New(p.updateTime),
			ProviderResponse: string(raw),
			SeenBefore:       p.seenBefore,
			Environment:      p.environment,
		})
	}

	return &api.ValidatePurchaseResponse{
		ValidatedPurchases: validatedPurchases,
	}, nil
}

func ValidatePurchaseHuawei(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, config *IAPHuaweiConfig, inAppPurchaseData, signature string, persist bool) (*api.ValidatePurchaseResponse, error) {
	validation, data, raw, err := iap.ValidateReceiptHuawei(ctx, httpc, config.PublicKey, config.ClientID, config.ClientSecret, inAppPurchaseData, signature)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			if vErr, ok := errors.AsType[*iap.ValidationError](err); ok {
				logger.Debug("Error validating Huawei receipt", zap.Error(vErr.Err), zap.Int("status_code", vErr.StatusCode), zap.String("payload", vErr.Payload))
				return nil, vErr
			} else {
				logger.Error("Error validating Huawei receipt", zap.Error(err))
			}
		}
		return nil, err
	}

	if validation.ResponseCode != strconv.Itoa(iap.HuaweiReceiptIsValid) {
		return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("Invalid Receipt. Code: %s", validation.ResponseCode))
	}

	env := api.StoreEnvironment_PRODUCTION
	if data.PurchaseType == iap.HuaweiSandboxPurchaseType {
		env = api.StoreEnvironment_SANDBOX
	}

	sPurchase := &storagePurchase{
		userID:        userID,
		store:         api.StoreProvider_HUAWEI_APP_GALLERY,
		productId:     validation.PurchaseTokenData.ProductId,
		transactionId: validation.PurchaseTokenData.PurchaseToken,
		rawResponse:   string(raw),
		purchaseTime:  parseMillisecondUnixTimestamp(data.PurchaseTime),
		environment:   env,
	}

	if !persist {
		validatedPurchases := []*api.ValidatedPurchase{
			{
				ProductId:        sPurchase.productId,
				TransactionId:    sPurchase.transactionId,
				Store:            sPurchase.store,
				PurchaseTime:     timestamppb.New(sPurchase.purchaseTime),
				ProviderResponse: string(raw),
				Environment:      sPurchase.environment,
			},
		}

		return &api.ValidatePurchaseResponse{ValidatedPurchases: validatedPurchases}, nil
	}

	purchases, err := upsertPurchases(ctx, db, []*storagePurchase{sPurchase})
	if err != nil {
		if err != context.Canceled {
			logger.Error("Error storing Huawei receipt", zap.Error(err))
		}
		return nil, err
	}

	validatedPurchases := make([]*api.ValidatedPurchase, 0, len(purchases))
	for _, p := range purchases {
		suid := p.userID.String()
		if p.userID.IsNil() {
			suid = ""
		}
		validatedPurchases = append(validatedPurchases, &api.ValidatedPurchase{
			UserId:           suid,
			ProductId:        p.productId,
			TransactionId:    p.transactionId,
			Store:            p.store,
			PurchaseTime:     timestamppb.New(p.purchaseTime),
			CreateTime:       timestamppb.New(p.createTime),
			UpdateTime:       timestamppb.New(p.updateTime),
			ProviderResponse: string(raw),
			SeenBefore:       p.seenBefore,
			Environment:      p.environment,
		})
	}

	return &api.ValidatePurchaseResponse{
		ValidatedPurchases: validatedPurchases,
	}, nil
}

func ValidatePurchaseFacebookInstant(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, config *IAPFacebookInstantConfig, signedRequest string, persist bool) (*api.ValidatePurchaseResponse, error) {
	payment, rawResponse, err := iap.ValidateReceiptFacebookInstant(config.AppSecret, signedRequest)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("Error validating Facebook Instant receipt", zap.Error(err))
		}
		return nil, err
	}

	sPurchase := &storagePurchase{
		userID:        userID,
		store:         api.StoreProvider_FACEBOOK_INSTANT_STORE,
		productId:     payment.ProductId,
		transactionId: payment.PurchaseToken,
		rawResponse:   rawResponse,
		purchaseTime:  time.Unix(int64(payment.PurchaseTime), 0),
		environment:   api.StoreEnvironment_PRODUCTION,
	}

	if !persist {
		validatedPurchases := []*api.ValidatedPurchase{
			{
				UserId:           userID.String(),
				ProductId:        sPurchase.productId,
				TransactionId:    sPurchase.transactionId,
				Store:            sPurchase.store,
				PurchaseTime:     timestamppb.New(sPurchase.purchaseTime),
				ProviderResponse: rawResponse,
				Environment:      sPurchase.environment,
			},
		}

		return &api.ValidatePurchaseResponse{ValidatedPurchases: validatedPurchases}, nil
	}

	purchases, err := upsertPurchases(ctx, db, []*storagePurchase{sPurchase})
	if err != nil {
		if err != context.Canceled {
			logger.Error("Error storing Facebook Instant receipt", zap.Error(err))
		}
		return nil, err
	}

	validatedPurchases := make([]*api.ValidatedPurchase, 0, len(purchases))
	for _, p := range purchases {
		suid := p.userID.String()
		if p.userID.IsNil() {
			suid = ""
		}
		validatedPurchases = append(validatedPurchases, &api.ValidatedPurchase{
			UserId:           suid,
			ProductId:        p.productId,
			TransactionId:    p.transactionId,
			Store:            p.store,
			PurchaseTime:     timestamppb.New(p.purchaseTime),
			CreateTime:       timestamppb.New(p.createTime),
			UpdateTime:       timestamppb.New(p.updateTime),
			ProviderResponse: rawResponse,
			SeenBefore:       p.seenBefore,
			Environment:      p.environment,
		})
	}

	return &api.ValidatePurchaseResponse{
		ValidatedPurchases: validatedPurchases,
	}, nil
}

func ValidatePurchaseSamsung(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, config *IAPSamsungConfig, purchaseId string, persist bool) (*api.ValidatePurchaseResponse, error) {
	packageName := ""
	if config != nil {
		packageName = config.GetPackageName()
	}

	orderResp, raw, err := iap.ValidateReceiptSamsung(ctx, httpc, packageName, purchaseId)
	if err != nil {
		if err != context.Canceled {
			if vErr, ok := errors.AsType[*iap.ValidationError](err); ok {
				logger.Debug("Error validating Samsung receipt", zap.Error(vErr.Err), zap.Int("status_code", vErr.StatusCode), zap.String("payload", vErr.Payload))
				return nil, vErr
			} else {
				logger.Error("Error validating Samsung receipt", zap.Error(err))
			}
		}
		return nil, err
	}

	env := api.StoreEnvironment_PRODUCTION
	if orderResp.IsSandbox() {
		env = api.StoreEnvironment_SANDBOX
	}

	sPurchase := &storagePurchase{
		userID:        userID,
		store:         api.StoreProvider_SAMSUNG_GALAXY_STORE,
		productId:     orderResp.ItemId,
		transactionId: purchaseId,
		rawResponse:   string(raw),
		purchaseTime:  iap.ParseSamsungPurchaseDate(orderResp.PurchaseDate),
		environment:   env,
	}

	if !persist {
		return &api.ValidatePurchaseResponse{
			ValidatedPurchases: []*api.ValidatedPurchase{
				{
					ProductId:        sPurchase.productId,
					TransactionId:    sPurchase.transactionId,
					Store:            sPurchase.store,
					PurchaseTime:     timestamppb.New(sPurchase.purchaseTime),
					ProviderResponse: string(raw),
					Environment:      sPurchase.environment,
				},
			},
		}, nil
	}

	purchases, err := upsertPurchases(ctx, db, []*storagePurchase{sPurchase})
	if err != nil {
		if err != context.Canceled {
			logger.Error("Error storing Samsung receipt", zap.Error(err))
		}
		return nil, err
	}

	validatedPurchases := make([]*api.ValidatedPurchase, 0, len(purchases))
	for _, p := range purchases {
		suid := p.userID.String()
		if p.userID.IsNil() {
			suid = ""
		}
		validatedPurchases = append(validatedPurchases, &api.ValidatedPurchase{
			UserId:           suid,
			ProductId:        p.productId,
			TransactionId:    p.transactionId,
			Store:            p.store,
			PurchaseTime:     timestamppb.New(p.purchaseTime),
			CreateTime:       timestamppb.New(p.createTime),
			UpdateTime:       timestamppb.New(p.updateTime),
			ProviderResponse: string(raw),
			SeenBefore:       p.seenBefore,
			Environment:      p.environment,
		})
	}

	return &api.ValidatePurchaseResponse{
		ValidatedPurchases: validatedPurchases,
	}, nil
}

func GetPurchaseByTransactionId(ctx context.Context, logger *zap.Logger, db *sql.DB, transactionID string) (*api.ValidatedPurchase, error) {

	return scanPurchaseMySQL(db.QueryRowContext(ctx, purchaseSelectMySQL+" WHERE transaction_id = ?", transactionID))

}

type purchasesListCursor struct {
	Filter        string
	TransactionId string
	PurchaseTime  *timestamppb.Timestamp
	UserId        string
	IsNext        bool
	After         time.Time
	Before        time.Time
}

func ListPurchases(ctx context.Context, logger *zap.Logger, db *sql.DB, userID string, limit int, cursor string, after, before time.Time) (*api.PurchaseList, error) {

	return listPurchasesMySQL(ctx, db, userID, limit, cursor, after, before)

}

type storagePurchase struct {
	userID        uuid.UUID
	store         api.StoreProvider
	productId     string
	transactionId string
	rawResponse   string
	purchaseTime  time.Time
	createTime    time.Time // Set by upsertPurchases
	updateTime    time.Time // Set by upsertPurchases
	refundTime    time.Time
	environment   api.StoreEnvironment
	seenBefore    bool // Set by upsertPurchases
}

func upsertPurchases(ctx context.Context, db *sql.DB, purchases []*storagePurchase) ([]*storagePurchase, error) {

	return upsertPurchasesMySQL(ctx, db, purchases)

}

func parseMillisecondUnixTimestamp(t int64) time.Time {
	return time.Unix(0, 0).Add(time.Duration(t) * time.Millisecond)
}
