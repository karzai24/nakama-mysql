// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func authenticateDeviceMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, deviceID, username string, create bool) (string, string, bool, error) {
	lookup := func() (string, string, bool, error) {
		var id, name string
		var disabled time.Time
		err := db.QueryRowContext(ctx, `SELECT u.id, u.username, u.disable_time
            FROM users u JOIN user_device d ON d.user_id = u.id WHERE d.provider = '' AND d.id = ?`, deviceID).Scan(&id, &name, &disabled)
		if err != nil {
			return "", "", false, err
		}
		if disabled.Unix() != 0 {
			return "", "", false, status.Error(codes.PermissionDenied, "User account banned.")
		}
		return id, name, false, nil
	}
	id, name, _, err := lookup()
	if err == nil || status.Code(err) == codes.PermissionDenied {
		return id, name, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		logger.Error("MySQL device lookup failed", zap.Error(err))
		return "", "", false, status.Error(codes.Internal, "Error finding user account.")
	}
	if !create {
		return "", "", false, status.Error(codes.NotFound, "User account not found.")
	}
	id = uuid.Must(uuid.NewV4()).String()
	err = executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO users (id, username) VALUES (?, ?)", id, username); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO user_device (id, user_id) VALUES (?, ?)", deviceID, id)
		return err
	})
	if err == nil {
		return id, username, true, nil
	}
	if mysqlErrorNumber(err, 1062) {
		// A competing create may have won either unique constraint. The losing
		// transaction has rolled back, so it cannot leave an orphan account.
		existingID, existingName, _, lookupErr := lookup()
		if lookupErr == nil || status.Code(lookupErr) == codes.PermissionDenied {
			return existingID, existingName, false, lookupErr
		}
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return "", "", false, status.Error(codes.AlreadyExists, "Username is already in use.")
		}
	}
	logger.Error("MySQL device authentication transaction failed", zap.Error(err))
	return "", "", false, status.Error(codes.Internal, "Error finding or creating user account.")
}
