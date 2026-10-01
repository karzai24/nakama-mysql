// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/social"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Constructed only after the existing social client has verified provider proof.
// column is an internal allowlisted identifier; all profile values are bound.
type identityProviderProfile struct {
	column, id, originalID, email, displayName, avatarURL string
}

func identityProviderColumn(column string) bool {
	switch column {
	case "apple_id", "facebook_id", "facebook_instant_game_id", "google_id", "gamecenter_id", "steam_id":
		return true
	default:
		return false
	}
}

func identityGoogleProfile(profile social.GoogleProfile) identityProviderProfile {
	p := identityProviderProfile{column: "google_id", id: profile.GetGoogleId(), originalID: profile.GetOriginalGoogleId(), email: profile.GetEmail()}
	// Retain upstream's byte limits and omission of oversized Google fields.
	if len(profile.GetDisplayName()) <= 255 {
		p.displayName = profile.GetDisplayName()
	}
	if len(profile.GetAvatarImageUrl()) <= 512 {
		p.avatarURL = profile.GetAvatarImageUrl()
	}
	return p
}

type identityExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func remapGoogleIDMySQL(ctx context.Context, executor identityExecer, id, originalID string) error {
	if originalID == "" || originalID == id {
		return nil
	}
	_, err := executor.ExecContext(ctx, "UPDATE users SET google_id = ? WHERE google_id = ?", id, originalID)
	return err
}

// A provider email is an optional import, never a reason to merge two accounts.
// InnoDB rolls back just the duplicate-key statement, so the surrounding identity
// transaction can still commit. Other failures roll back the whole transaction.
func importProviderEmailMySQL(ctx context.Context, tx *sql.Tx, owner, email string) error {
	if email == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, "UPDATE users SET email = ? WHERE id = ?", email, owner)
	if mysqlErrorNumber(err, 1062) {
		return nil
	}
	return err
}

func authenticateProviderMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, profile identityProviderProfile, username string, create bool) (string, string, bool, error) {
	if !identityProviderColumn(profile.column) {
		return "", "", false, status.Error(codes.Internal, "Unsupported authentication provider.")
	}
	if profile.id == "" {
		return "", "", false, status.Error(codes.Unauthenticated, "Provider account identifier is missing.")
	}
	lookup := func() (string, string, bool, error) {
		var id, name string
		var disabled time.Time
		err := db.QueryRowContext(ctx, "SELECT id, username, disable_time FROM users WHERE "+profile.column+" = ?", profile.id).Scan(&id, &name, &disabled)
		if err != nil {
			return "", "", false, err
		}
		if disabled.Unix() != 0 {
			return "", "", false, status.Error(codes.PermissionDenied, "User account banned.")
		}
		if profile.column == "google_id" && (profile.displayName != "" || profile.avatarURL != "") {
			// Fill absent fields without overwriting a concurrent player update.
			_, err = db.ExecContext(ctx, `UPDATE users SET
                display_name = COALESCE(NULLIF(display_name, ''), ?),
                avatar_url = COALESCE(NULLIF(avatar_url, ''), ?), update_time = CURRENT_TIMESTAMP(6)
                WHERE id = ? AND ((COALESCE(display_name, '') = '' AND ? <> '') OR (COALESCE(avatar_url, '') = '' AND ? <> ''))`,
				profile.displayName, profile.avatarURL, id, profile.displayName, profile.avatarURL)
			if err != nil {
				logger.Error("MySQL Google profile fill failed", zap.Error(err))
			}
		}
		return id, name, false, nil
	}
	id, name, _, err := lookup()
	if errors.Is(err, sql.ErrNoRows) && profile.column == "google_id" && profile.originalID != "" && profile.originalID != profile.id {
		if err = remapGoogleIDMySQL(ctx, db, profile.id, profile.originalID); err == nil {
			id, name, _, err = lookup()
		}
	}
	if err == nil || status.Code(err) == codes.PermissionDenied {
		return id, name, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		// Driver errors may contain a colliding provider ID; keep it out of logs.
		logger.Error("MySQL provider account lookup or remap failed", zap.String("provider", profile.column))
		return "", "", false, status.Error(codes.Internal, "Error finding user account.")
	}
	if !create {
		return "", "", false, status.Error(codes.NotFound, "User account not found.")
	}
	id = uuid.Must(uuid.NewV4()).String()
	err = executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		var displayName, avatarURL any
		if profile.column == "google_id" || profile.column == "facebook_id" {
			displayName, avatarURL = profile.displayName, profile.avatarURL
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO users (id, username, "+profile.column+", display_name, avatar_url) VALUES (?, ?, ?, ?, ?)", id, username, profile.id, displayName, avatarURL)
		if err != nil {
			return err
		}
		return importProviderEmailMySQL(ctx, tx, id, profile.email)
	})
	if err == nil {
		return id, username, true, nil
	}
	if mysqlErrorNumber(err, 1062) {
		id, name, _, lookupErr := lookup()
		if lookupErr == nil || status.Code(lookupErr) == codes.PermissionDenied {
			return id, name, false, lookupErr
		}
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return "", "", false, status.Error(codes.AlreadyExists, "Username is already in use.")
		}
	}
	logger.Error("MySQL provider account creation failed", zap.String("provider", profile.column))
	return "", "", false, status.Error(codes.Internal, "Error finding or creating user account.")
}

func linkProviderMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, profile identityProviderProfile) error {
	if !identityProviderColumn(profile.column) {
		return status.Error(codes.Internal, "Unsupported authentication provider.")
	}
	if profile.id == "" {
		return status.Error(codes.Unauthenticated, "Provider account identifier is missing.")
	}
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		var owner string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", userID.String()).Scan(&owner); err != nil {
			return err
		}
		if profile.column == "google_id" {
			if err := remapGoogleIDMySQL(ctx, tx, profile.id, profile.originalID); err != nil {
				return err
			}
		}
		query := "UPDATE users SET " + profile.column + " = ?, update_time = CURRENT_TIMESTAMP(6)"
		args := []any{profile.id}
		if profile.column == "facebook_id" || profile.column == "google_id" {
			query += ", display_name = COALESCE(NULLIF(display_name, ''), ?), avatar_url = COALESCE(NULLIF(avatar_url, ''), ?)"
			args = append(args, profile.displayName, profile.avatarURL)
		}
		args = append(args, owner)
		if _, err := tx.ExecContext(ctx, query+" WHERE id = ?", args...); err != nil {
			return err
		}
		return importProviderEmailMySQL(ctx, tx, owner, profile.email)
	})
	if mysqlErrorNumber(err, 1062) || errors.Is(err, sql.ErrNoRows) {
		return status.Error(codes.AlreadyExists, "Provider ID is already in use.")
	}
	if err != nil {
		logger.Error("MySQL provider link failed", zap.String("provider", profile.column))
		return status.Error(codes.Internal, "Error while trying to link provider ID.")
	}
	return nil
}
