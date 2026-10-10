// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func authenticateCustomMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, customID, username string, create bool) (string, string, bool, error) {
	lookup := func() (string, string, bool, error) {
		var id, name string
		var disabled time.Time
		if err := db.QueryRowContext(ctx, "SELECT id, username, disable_time FROM users WHERE custom_id = ?", customID).Scan(&id, &name, &disabled); err != nil {
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
		logger.Error("MySQL custom account lookup failed", zap.Error(err))
		return "", "", false, status.Error(codes.Internal, "Error finding user account.")
	}
	if !create {
		return "", "", false, status.Error(codes.NotFound, "User account not found.")
	}
	id = uuid.Must(uuid.NewV4()).String()
	_, err = db.ExecContext(ctx, "INSERT INTO users (id, username, custom_id) VALUES (?, ?, ?)", id, username, customID)
	if err == nil {
		return id, username, true, nil
	}
	if mysqlErrorNumber(err, 1062) {
		// A concurrent creator may have won the identity constraint. Re-read the
		// account (including its ban state); never overwrite its username.
		id, name, _, lookupErr := lookup()
		if lookupErr == nil || status.Code(lookupErr) == codes.PermissionDenied {
			return id, name, false, lookupErr
		}
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return "", "", false, status.Error(codes.AlreadyExists, "Username is already in use.")
		}
		// Duplicate-key error text can contain an identity. Do not log it.
		err = lookupErr
	}
	logger.Error("MySQL custom account creation failed", zap.Error(err))
	return "", "", false, status.Error(codes.Internal, "Error finding or creating user account.")
}

type passwordAccount struct {
	id       string
	username string
	password []byte
	disabled time.Time
}

func findPasswordAccountMySQL(ctx context.Context, db *sql.DB, query, identifier string) (*passwordAccount, error) {
	account := &passwordAccount{}
	err := db.QueryRowContext(ctx, query, identifier).Scan(&account.id, &account.username, &account.password, &account.disabled)
	return account, err
}

func (a *passwordAccount) authenticate(password string) (string, string, bool, error) {
	if a.disabled.Unix() != 0 {
		return "", "", false, status.Error(codes.PermissionDenied, "User account banned.")
	}
	// A provider-imported email with no password is not a password login.
	// Preserve the same public error as a wrong password.
	if len(a.password) == 0 || bcrypt.CompareHashAndPassword(a.password, []byte(password)) != nil {
		return "", "", false, status.Error(codes.Unauthenticated, "Invalid credentials.")
	}
	return a.id, a.username, false, nil
}

func authenticateEmailMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, email, password, username string, create bool) (string, string, bool, error) {
	const query = "SELECT id, username, password, disable_time FROM users WHERE email = ?"
	account, err := findPasswordAccountMySQL(ctx, db, query, email)
	if err == nil {
		return account.authenticate(password)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		logger.Error("MySQL email account lookup failed", zap.Error(err))
		return "", "", false, status.Error(codes.Internal, "Error finding user account.")
	}
	if !create {
		return "", "", false, status.Error(codes.NotFound, "User account not found.")
	}
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcryptHashCost)
	if err != nil {
		logger.Error("MySQL account password hashing failed", zap.Error(err))
		return "", "", false, status.Error(codes.Internal, "Error finding or creating user account.")
	}
	id := uuid.Must(uuid.NewV4()).String()
	_, err = db.ExecContext(ctx, "INSERT INTO users (id, username, email, password) VALUES (?, ?, ?, ?)", id, username, email, hashedPassword)
	if err == nil {
		return id, username, true, nil
	}
	if mysqlErrorNumber(err, 1062) {
		account, lookupErr := findPasswordAccountMySQL(ctx, db, query, email)
		if lookupErr == nil {
			// A race on email is never evidence that the loser knows the winner's
			// password. Authenticate against the committed hash before returning.
			return account.authenticate(password)
		}
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return "", "", false, status.Error(codes.AlreadyExists, "Username is already in use.")
		}
		err = lookupErr
	}
	logger.Error("MySQL email account creation failed", zap.Error(err))
	return "", "", false, status.Error(codes.Internal, "Error finding or creating user account.")
}

func authenticateUsernameMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, username, password string) (string, error) {
	account, err := findPasswordAccountMySQL(ctx, db, "SELECT id, username, password, disable_time FROM users WHERE username = ?", username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", status.Error(codes.NotFound, "User account not found.")
	}
	if err != nil {
		logger.Error("MySQL username account lookup failed", zap.Error(err))
		return "", status.Error(codes.Internal, "Error finding user account.")
	}
	id, _, _, err := account.authenticate(password)
	return id, err
}
