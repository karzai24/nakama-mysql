// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/gob"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const purchaseSelectMySQL = `SELECT
			user_id,
			store,
			transaction_id,
			create_time,
			update_time,
			purchase_time,
			refund_time,
			product_id,
			environment,
			raw_response
		FROM purchase`

func scanPurchaseMySQL(row Scannable) (*api.ValidatedPurchase, error) {
	var (
		dbTransactionId string
		dbUserId        uuid.UUID
		dbStore         api.StoreProvider
		dbCreateTime    sql.NullTime
		dbUpdateTime    sql.NullTime
		dbPurchaseTime  sql.NullTime
		dbRefundTime    sql.NullTime
		dbProductId     string
		dbEnvironment   api.StoreEnvironment
		dbRawResponse   string
	)

	err := row.Scan(&dbUserId, &dbStore, &dbTransactionId, &dbCreateTime, &dbUpdateTime, &dbPurchaseTime, &dbRefundTime, &dbProductId, &dbEnvironment, &dbRawResponse)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Not found
			return nil, nil
		}
		return nil, err
	}

	suid := dbUserId.String()
	if dbUserId.IsNil() {
		suid = ""
	}

	return &api.ValidatedPurchase{
		UserId:           suid,
		ProductId:        dbProductId,
		TransactionId:    dbTransactionId,
		Store:            dbStore,
		PurchaseTime:     timestamppb.New(dbPurchaseTime.Time),
		CreateTime:       timestamppb.New(dbCreateTime.Time),
		UpdateTime:       timestamppb.New(dbUpdateTime.Time),
		Environment:      dbEnvironment,
		RefundTime:       timestamppb.New(dbRefundTime.Time),
		ProviderResponse: dbRawResponse,
	}, nil
}

const subscriptionSelectMySQL = `SELECT
		    user_id,
				store,
				original_transaction_id,
				create_time,
				update_time,
				expire_time,
				purchase_time,
				refund_time,
				product_id,
				environment,
				raw_response,
				raw_notification
		FROM subscription`

func scanSubscriptionMySQL(row Scannable) (*api.ValidatedSubscription, error) {
	var (
		dbUserId                uuid.UUID
		dbStore                 api.StoreProvider
		dbOriginalTransactionId string
		dbCreateTime            sql.NullTime
		dbUpdateTime            sql.NullTime
		dbExpireTime            sql.NullTime
		dbPurchaseTime          sql.NullTime
		dbRefundTime            sql.NullTime
		dbProductId             string
		dbEnvironment           api.StoreEnvironment
		dbRawResponse           string
		dbRawNotification       string
	)

	err := row.Scan(&dbUserId, &dbStore, &dbOriginalTransactionId, &dbCreateTime, &dbUpdateTime, &dbExpireTime, &dbPurchaseTime, &dbRefundTime, &dbProductId, &dbEnvironment, &dbRawResponse, &dbRawNotification)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Not found
			return nil, nil
		}
		return nil, err
	}

	active := false
	if dbExpireTime.Time.After(time.Now()) && dbRefundTime.Time.Unix() == 0 {
		active = true
	}

	suid := dbUserId.String()
	if dbUserId.IsNil() {
		suid = ""
	}

	return &api.ValidatedSubscription{
		UserId:                suid,
		ProductId:             dbProductId,
		OriginalTransactionId: dbOriginalTransactionId,
		Store:                 dbStore,
		PurchaseTime:          timestamppb.New(dbPurchaseTime.Time),
		CreateTime:            timestamppb.New(dbCreateTime.Time),
		UpdateTime:            timestamppb.New(dbUpdateTime.Time),
		Environment:           dbEnvironment,
		ExpiryTime:            timestamppb.New(dbExpireTime.Time),
		RefundTime:            timestamppb.New(dbRefundTime.Time),
		ProviderResponse:      dbRawResponse,
		ProviderNotification:  dbRawNotification,
		Active:                active,
	}, nil
}

type billingCursorMySQL struct {
	Kind, Filter, Owner, ID string
	Time, After, Before     time.Time
	Next                    bool
}

func billingQueryMySQL(kind, owner string, limit int, encoded string, after, before time.Time) (string, []any, *billingCursorMySQL, int, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 10000 {
		return "", nil, nil, 0, status.Error(codes.InvalidArgument, "Invalid limit.")
	}
	if owner != "" {
		id, err := uuid.FromString(owner)
		if err != nil {
			return "", nil, nil, 0, status.Error(codes.InvalidArgument, "Invalid owner.")
		}
		owner = id.String()
	}
	var c *billingCursorMySQL
	if encoded != "" {
		c = &billingCursorMySQL{}
		b, err := base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			return "", nil, nil, 0, status.Error(codes.InvalidArgument, "Invalid cursor.")
		}
		if err = gob.NewDecoder(bytes.NewReader(b)).Decode(c); err != nil || c.Kind != kind || owner != c.Filter || !after.Equal(c.After) || !before.Equal(c.Before) {
			return "", nil, nil, 0, status.Error(codes.InvalidArgument, "Cursor does not match filters.")
		}
	}
	q := purchaseSelectMySQL
	idColumn := "transaction_id"
	if kind == "subscription" {
		q = subscriptionSelectMySQL
		idColumn = "original_transaction_id"
	}
	where := []string{}
	args := []any{}
	order := "DESC"
	if owner != "" {
		where = append(where, "user_id = ?")
		args = append(args, owner)
	}
	if c != nil {
		op := "<"
		if !c.Next {
			op = ">"
			order = "ASC"
		}
		where = append(where, "(purchase_time,user_id,"+idColumn+") "+op+" (?,?,?)")
		args = append(args, c.Time, c.Owner, c.ID)
	}
	if !after.IsZero() {
		where = append(where, "purchase_time >= ?")
		args = append(args, after)
	}
	if !before.IsZero() {
		where = append(where, "purchase_time <= ?")
		args = append(args, before)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY purchase_time " + order + ",user_id " + order + "," + idColumn + " " + order + " LIMIT ?"
	args = append(args, limit+1)
	return q, args, c, limit, nil
}
func billingCursorsMySQL(points []*billingCursorMySQL, c *billingCursorMySQL, more bool) (string, string, error) {
	if len(points) == 0 {
		return "", "", nil
	}
	var next, prev *billingCursorMySQL
	if c != nil && !c.Next {
		if more {
			prev = points[0]
		}
		next = points[len(points)-1]
	} else {
		if more {
			next = points[len(points)-1]
		}
		if c != nil {
			prev = points[0]
		}
	}
	encode := func(p *billingCursorMySQL, isNext bool) (string, error) {
		if p == nil {
			return "", nil
		}
		p.Next = isNext
		b := &bytes.Buffer{}
		if err := gob.NewEncoder(b).Encode(p); err != nil {
			return "", err
		}
		return base64.URLEncoding.EncodeToString(b.Bytes()), nil
	}
	n, err := encode(next, true)
	if err != nil {
		return "", "", err
	}
	p, err := encode(prev, false)
	return n, p, err
}
func listPurchasesMySQL(ctx context.Context, db *sql.DB, owner string, limit int, cursor string, after, before time.Time) (*api.PurchaseList, error) {
	if owner != "" {
		id, e := uuid.FromString(owner)
		if e != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid owner.")
		}
		owner = id.String()
	}
	q, args, c, limit, err := billingQueryMySQL("purchase", owner, limit, cursor, after, before)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &api.PurchaseList{}
	points := []*billingCursorMySQL{}
	more := false
	for rows.Next() {
		if len(points) == limit {
			more = true
			break
		}
		p, err := scanPurchaseMySQL(rows)
		if err != nil {
			return nil, err
		}
		out.ValidatedPurchases = append(out.ValidatedPurchases, p)
		uid := p.UserId
		if uid == "" {
			uid = uuid.Nil.String()
		}
		points = append(points, &billingCursorMySQL{Kind: "purchase", Filter: owner, Owner: uid, ID: p.TransactionId, Time: p.PurchaseTime.AsTime(), After: after, Before: before})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if c != nil && !c.Next {
		slices.Reverse(points)
		slices.Reverse(out.ValidatedPurchases)
	}
	out.Cursor, out.PrevCursor, err = billingCursorsMySQL(points, c, more)
	return out, err
}
func listSubscriptionsMySQL(ctx context.Context, db *sql.DB, owner string, limit int, cursor string, after, before time.Time) (*api.SubscriptionList, error) {
	if owner != "" {
		id, e := uuid.FromString(owner)
		if e != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid owner.")
		}
		owner = id.String()
	}
	q, args, c, limit, err := billingQueryMySQL("subscription", owner, limit, cursor, after, before)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &api.SubscriptionList{}
	points := []*billingCursorMySQL{}
	more := false
	for rows.Next() {
		if len(points) == limit {
			more = true
			break
		}
		p, err := scanSubscriptionMySQL(rows)
		if err != nil {
			return nil, err
		}
		out.ValidatedSubscriptions = append(out.ValidatedSubscriptions, p)
		uid := p.UserId
		if uid == "" {
			uid = uuid.Nil.String()
		}
		points = append(points, &billingCursorMySQL{Kind: "subscription", Filter: owner, Owner: uid, ID: p.OriginalTransactionId, Time: p.PurchaseTime.AsTime(), After: after, Before: before})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if c != nil && !c.Next {
		slices.Reverse(points)
		slices.Reverse(out.ValidatedSubscriptions)
	}
	out.Cursor, out.PrevCursor, err = billingCursorsMySQL(points, c, more)
	return out, err
}
