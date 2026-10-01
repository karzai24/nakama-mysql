// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDualBackendConsoleNotificationsAndBilling(t *testing.T) {
	s, ctx := consoleFixture(t)
	u, _ := lifecycleUser(t, s.db)
	other, _ := lifecycleUser(t, s.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	var firstNotification string
	for i := 0; i < 5; i++ {
		id := uuid.Must(uuid.NewV4()).String()
		if i == 0 {
			firstNotification = id
		}
		lifecycleExec(t, s.db, "INSERT INTO notification (id,user_id,subject,content,code,sender_id,create_time) VALUES (?,?,'test','{}',1,?,?)", id, u, uuid.Nil, now)
		transaction := "console_" + GenerateString()
		lifecycleExec(t, s.db, "INSERT INTO purchase (transaction_id,user_id,product_id,purchase_time) VALUES (?,?,'product',?)", transaction, u, now)
		lifecycleExec(t, s.db, "INSERT INTO subscription (original_transaction_id,user_id,product_id,purchase_time,expire_time) VALUES (?,?,'product',?,?)", transaction, u, now, now.Add(time.Hour))
		t.Cleanup(func() {
			lifecycleExec(t, s.db, "DELETE FROM purchase WHERE transaction_id = ?", transaction)
			lifecycleExec(t, s.db, "DELETE FROM subscription WHERE original_transaction_id = ?", transaction)
		})
	}
	p, err := s.ListNotifications(ctx, &console.ListNotificationsRequest{UserId: u.String(), Limit: 2})
	require.NoError(t, err)
	require.Len(t, p.Notifications, 2)
	n, err := s.ListNotifications(ctx, &console.ListNotificationsRequest{UserId: u.String(), Limit: 2, Cursor: p.NextCursor})
	require.NoError(t, err)
	require.Len(t, n.Notifications, 2)
	b, err := s.ListNotifications(ctx, &console.ListNotificationsRequest{UserId: u.String(), Limit: 2, Cursor: n.PrevCursor})
	require.NoError(t, err)
	require.Equal(t, p.Notifications, b.Notifications)
	{
		_, err = s.ListNotifications(ctx, &console.ListNotificationsRequest{UserId: other.String(), Limit: 2, Cursor: p.NextCursor})
		require.Error(t, err)
	}
	notification, err := s.GetNotification(ctx, &console.GetNotificationRequest{Id: firstNotification})
	require.NoError(t, err)
	require.Equal(t, u.String(), notification.UserId)
	_, err = s.DeleteNotification(ctx, &console.DeleteNotificationRequest{Id: firstNotification})
	require.NoError(t, err)
	purchases, err := s.ListPurchases(ctx, &console.ListPurchasesRequest{UserId: u.String(), Limit: 2})
	require.NoError(t, err)
	require.Len(t, purchases.ValidatedPurchases, 2)
	purchase, err := s.GetPurchase(ctx, &console.GetPurchaseRequest{TransactionId: purchases.ValidatedPurchases[0].TransactionId})
	require.NoError(t, err)
	require.Equal(t, u.String(), purchase.UserId)
	subs, err := s.ListSubscriptions(ctx, &console.ListSubscriptionsRequest{UserId: u.String(), Limit: 2})
	require.NoError(t, err)
	require.Len(t, subs.ValidatedSubscriptions, 2)
	sub, err := s.GetSubscription(ctx, &console.GetSubscriptionRequest{OriginalTransactionId: subs.ValidatedSubscriptions[0].OriginalTransactionId})
	require.NoError(t, err)
	require.True(t, sub.Active)
	{
		next, err := s.ListSubscriptions(ctx, &console.ListSubscriptionsRequest{UserId: u.String(), Limit: 2, Cursor: subs.Cursor})
		require.NoError(t, err)
		require.Len(t, next.ValidatedSubscriptions, 2)
		back, err := s.ListSubscriptions(ctx, &console.ListSubscriptionsRequest{UserId: u.String(), Limit: 2, Cursor: next.PrevCursor})
		require.NoError(t, err)
		require.Equal(t, subs.ValidatedSubscriptions, back.ValidatedSubscriptions)
		last, err := s.ListSubscriptions(ctx, &console.ListSubscriptionsRequest{UserId: u.String(), Limit: 2, Cursor: next.Cursor})
		require.NoError(t, err)
		require.Len(t, last.ValidatedSubscriptions, 1)
		require.Empty(t, last.Cursor)
		filter := &console.ListPurchasesRequest{UserId: u.String(), Limit: 2, After: timestamppb.New(now.Add(-time.Minute)), Before: timestamppb.New(now.Add(time.Minute))}
		p, err := s.ListPurchases(ctx, filter)
		require.NoError(t, err)
		filter.Cursor = p.Cursor
		n, err := s.ListPurchases(ctx, filter)
		require.NoError(t, err)
		filter.Cursor = n.PrevCursor
		b, err := s.ListPurchases(ctx, filter)
		require.NoError(t, err)
		require.Equal(t, p.ValidatedPurchases, b.ValidatedPurchases)
		filter.UserId = other.String()
		_, err = s.ListPurchases(ctx, filter)
		require.Error(t, err)
	}
}
func TestDualBackendConsoleDestructiveMaintenance(t *testing.T) {
	requireDisposableFaultTests(t)
	s, ctx := consoleFixture(t)
	var db *sql.DB
	{
		address := os.Getenv("TEST_MYSQL_ADMIN_URL")
		if address == "" {
			t.Skip("requires disposable MySQL database")
		}
		cfg := NewConfig(s.logger)
		cfg.GetDatabase().Addresses = []string{address}
		c, err := DbMySQLConfig(cfg)
		require.NoError(t, err)
		c.DBName = ""
		admin, err := openMySQL(c)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, admin.Close()) })
		name := fmt.Sprintf("nakama_console_test_%d", time.Now().UnixNano())
		_, err = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin")
		require.NoError(t, err)
		t.Cleanup(func() { _, err := admin.Exec("DROP DATABASE `" + name + "`"); require.NoError(t, err) })
		c.DBName = name
		db, err = openMySQL(c)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		var actual string
		require.NoError(t, db.QueryRow("SELECT DATABASE()").Scan(&actual))
		require.Equal(t, name, actual)
		require.NotEqual(t, "nakama", actual)
		files, err := filepath.Glob("../migrate/mysql/*.up.sql")
		require.NoError(t, err)
		require.NotEmpty(t, files)
		for _, file := range files {
			schema, err := os.ReadFile(file)
			require.NoError(t, err)
			for _, statement := range strings.Split(string(schema), "-- statement-break") {
				if strings.TrimSpace(statement) != "" {
					_, err = db.Exec(statement)
					require.NoError(t, err)
				}
			}
		}

	}

	s.db = db
	s.tracker = nil
	s.sessionRegistry = nil
	reset := func(storageOnly bool) (*emptypb.Empty, error) {
		if storageOnly {
			return s.DeleteStorage(ctx, &emptypb.Empty{})
		}
		return s.DeleteAllData(ctx, &emptypb.Empty{})
	}
	user := uuid.Must(uuid.NewV4())
	board := "board"
	note := uuid.Must(uuid.NewV4())
	purchase := "purchase"
	lifecycleExec(t, db, "INSERT INTO users (id,username,custom_id) VALUES (?,'test','test-custom')", user)
	lifecycleExec(t, db, "INSERT INTO storage (user_id,collection,`key`,`value`,version) VALUES (?,'test','key','{}','v')", user)
	lifecycleExec(t, db, "INSERT INTO leaderboard (id) VALUES (?)", board)
	lifecycleExec(t, db, "INSERT INTO leaderboard_record (leaderboard_id,owner_id) VALUES (?,?)", board, user)
	lifecycleExec(t, db, "INSERT INTO purchase (transaction_id,user_id,product_id) VALUES (?,?,'product')", purchase, user)
	lifecycleExec(t, db, "INSERT INTO users_notes (user_id,id,note) VALUES (?,?,'note')", user, note)
	drop := failureTrigger(t, db, "users_notes", "DELETE", "TRUE")
	index := s.storageIndex.(*walletTestIndex)
	_, err := reset(false)
	require.Error(t, err)
	require.Empty(t, index.deletes)
	require.Equal(t, 2, lifecycleCount(t, db, "SELECT COUNT(*) FROM users"))
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM storage"))
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM purchase"))
	drop()
	_, err = reset(true)
	require.NoError(t, err)
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM storage"))
	require.Len(t, index.deletes, 1)
	_, err = reset(false)
	require.NoError(t, err)
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM users"))
	for _, table := range []string{"storage", "purchase", "users_notes", "leaderboard", "leaderboard_record"} {
		require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM "+table))
	}
	var username string
	require.NoError(t, db.QueryRowContext(context.Background(), lifecycleSQL(db, "SELECT username FROM users WHERE id = ?"), uuid.Nil).Scan(&username))
	require.Empty(t, username)

	// The deprecated endpoint must use account lifecycle cleanup too, but retain
	// definitions and tombstoned receipts rather than performing a full reset.
	lifecycleExec(t, db, "INSERT INTO users (id,username,custom_id) VALUES (?,'deprecated','deprecated-custom')", user)
	lifecycleExec(t, db, "INSERT INTO storage (user_id,collection,`key`,`value`,version) VALUES (?,'test','key','{}','v')", user)
	lifecycleExec(t, db, "INSERT INTO purchase (transaction_id,user_id,product_id) VALUES (?,?,'product')", purchase, user)
	lifecycleExec(t, db, "INSERT INTO leaderboard (id) VALUES (?)", board)
	s.tracker = lifecycleTracker{}
	s.sessionRegistry = &lifecycleSessions{}
	_, err = s.DeleteAccounts(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM users"))
	require.Zero(t, lifecycleCount(t, db, "SELECT COUNT(*) FROM storage"))
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM leaderboard"))
	require.Equal(t, 1, lifecycleCount(t, db, "SELECT COUNT(*) FROM purchase WHERE user_id = ?", uuid.Nil))
}
