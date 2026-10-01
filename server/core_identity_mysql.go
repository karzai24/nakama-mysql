// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/gofrs/uuid/v5"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func linkEmailMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, email, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptHashCost)
	if err != nil {
		// In particular, bcrypt rejects passwords longer than 72 bytes. Never
		// replace an existing password with NULL on a hashing failure.
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return status.Error(codes.InvalidArgument, "Password must be at most 72 bytes long.")
		}
		logger.Error("MySQL link password hashing failed", zap.Error(err))
		return status.Error(codes.Internal, "Error while trying to link email.")
	}
	return linkUserIdentifierMySQL(ctx, logger, db, userID, "email", strings.ToLower(email), hash)
}

// kind is chosen by the native dispatch, never used as interpolated SQL.
func linkUserIdentifierMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, kind, value string, password []byte) error {
	var result sql.Result
	var err error
	var label string
	switch kind {
	case "custom_id":
		label = "Custom ID"
		result, err = db.ExecContext(ctx, "UPDATE users SET custom_id = ?, update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", value, userID.String())
	case "email":
		label = "Email"
		result, err = db.ExecContext(ctx, "UPDATE users SET email = ?, password = ?, update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", value, password, userID.String())
	default:
		return status.Error(codes.Internal, "Unsupported account identifier.")
	}
	// Unique constraints provide race-safe ownership. MySQL's clientFoundRows
	// setting makes re-linking an unchanged identifier succeed like PostgreSQL.
	if mysqlErrorNumber(err, 1062) {
		return status.Error(codes.AlreadyExists, label+" is already in use.")
	}
	if err == nil {
		var count int64
		count, err = result.RowsAffected()
		if err == nil && count == 0 {
			return status.Error(codes.AlreadyExists, label+" is already in use.")
		}
	}
	if err != nil {
		logger.Error("MySQL account identifier link failed", zap.Error(err))
		return status.Error(codes.Internal, "Error while trying to link "+label+".")
	}
	return nil
}

func linkDeviceMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, deviceID string) error {
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		var owner string
		// All device mutations take the account lock first. Unlink checks use
		// that same lock, preventing concurrent removal of the last two links.
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", userID.String()).Scan(&owner); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, "SELECT user_id FROM user_device WHERE provider = '' AND id = ?", deviceID).Scan(&owner)
		switch {
		case err == nil:
			if owner != userID.String() {
				return status.Error(codes.AlreadyExists, "Device ID already in use.")
			}
		case errors.Is(err, sql.ErrNoRows):
			if _, err = tx.ExecContext(ctx, "INSERT INTO user_device (id, user_id) VALUES (?, ?)", deviceID, userID.String()); err != nil {
				return err
			}
		default:
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE users SET update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", userID.String())
		return err
	})
	if err == nil || status.Code(err) == codes.AlreadyExists {
		return err
	}
	if mysqlErrorNumber(err, 1062) {
		return status.Error(codes.AlreadyExists, "Device ID already in use.")
	}
	logger.Error("MySQL device link failed", zap.Error(err))
	return status.Error(codes.Internal, "Error linking Device ID.")
}

func unlinkIdentifierMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, kind, value string) error {
	denied := status.Error(codes.PermissionDenied, "Cannot unlink last account identifier. Check profile exists and is not last link.")
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		var custom, email sql.NullString
		var password []byte
		var apple, facebook, instant, google, gamecenter, steam sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT custom_id, email, apple_id, facebook_id,
            facebook_instant_game_id, google_id, gamecenter_id, steam_id, password
            FROM users WHERE id = ? FOR UPDATE`, userID.String()).Scan(
			&custom, &email, &apple, &facebook, &instant, &google, &gamecenter, &steam, &password)
		if errors.Is(err, sql.ErrNoRows) {
			return denied
		}
		if err != nil {
			return err
		}
		var devices, matchingDevice int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(provider = '' AND id = ?), 0)
            FROM user_device WHERE user_id = ?`, value, userID.String()).Scan(&devices, &matchingDevice); err != nil {
			return err
		}
		identifiers := map[string]sql.NullString{
			"custom_id": custom, "email": email, "apple_id": apple, "facebook_id": facebook,
			"facebook_instant_game_id": instant, "google_id": google, "gamecenter_id": gamecenter, "steam_id": steam,
		}
		otherIdentifier := false
		for column, identifier := range identifiers {
			// An imported email without a password cannot keep an account accessible.
			if column != kind && identifier.Valid && (column != "email" || len(password) > 0) {
				otherIdentifier = true
			}
		}
		if kind == "device" {
			if matchingDevice == 0 || (!otherIdentifier && devices < 2) {
				return denied
			}
			_, err = tx.ExecContext(ctx, "DELETE FROM user_device WHERE provider = '' AND id = ? AND user_id = ?", value, userID.String())
			if err == nil {
				_, err = tx.ExecContext(ctx, "UPDATE users SET update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", userID.String())
			}
		} else {
			// Map membership is also the SQL column allowlist. No caller value is
			// interpolated as an identifier without this check.
			current, supported := identifiers[kind]
			if !supported {
				return status.Error(codes.Internal, "Unsupported account identifier.")
			}
			if (value != "" && (!current.Valid || current.String != value)) || (!otherIdentifier && devices == 0) {
				return denied
			}
			query := "UPDATE users SET " + kind + " = NULL, update_time = CURRENT_TIMESTAMP(6)"
			if kind == "email" {
				query += ", password = NULL"
			}
			_, err = tx.ExecContext(ctx, query+" WHERE id = ?", userID.String())
		}
		return err
	})
	if err == nil || status.Code(err) == codes.PermissionDenied {
		return err
	}
	logger.Error("MySQL account identifier unlink failed", zap.Error(err))
	return status.Error(codes.Internal, "Error while trying to unlink account identifier.")
}
