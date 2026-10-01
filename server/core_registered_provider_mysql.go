// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type mysqlAccountIdentity struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

func splitMySQLAccountIdentities(identities []mysqlAccountIdentity) ([]*api.AccountDevice, []*api.AccountProviderIdentity) {
	devices := make([]*api.AccountDevice, 0, len(identities))
	providers := make([]*api.AccountProviderIdentity, 0, len(identities))
	for _, identity := range identities {
		if identity.Provider == "" {
			devices = append(devices, &api.AccountDevice{Id: identity.ID})
		} else {
			providers = append(providers, &api.AccountProviderIdentity{Provider: identity.Provider, ProviderUserId: identity.ID})
		}
	}
	return devices, providers
}

func authenticateRegisteredProviderMySQL(ctx context.Context, db *sql.DB, provider, identity, requestedID, username string, create bool) (string, string, bool, error) {
	lookup := func() (string, string, error) {
		var id, name string
		var disabled time.Time
		err := db.QueryRowContext(ctx, "SELECT u.id,u.username,u.disable_time FROM users u JOIN user_device d ON u.id=d.user_id WHERE d.provider=? AND d.id=?", provider, identity).Scan(&id, &name, &disabled)
		if err == nil && disabled.Unix() != 0 {
			err = status.Error(codes.PermissionDenied, "User account banned.")
		}
		return id, name, err
	}
	id, name, err := lookup()
	if err == nil || status.Code(err) == codes.PermissionDenied {
		return id, name, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", false, status.Error(codes.Internal, "Error finding user account.")
	}
	if !create {
		return "", "", false, status.Error(codes.NotFound, "User account not found.")
	}
	id = requestedID
	if id == "" {
		if parsed, e := uuid.FromString(identity); e == nil {
			id = parsed.String()
		} else {
			id = uuid.Must(uuid.NewV4()).String()
		}
	}
	if parsed, e := uuid.FromString(id); e != nil {
		return "", "", false, status.Error(codes.InvalidArgument, "Invalid user ID.")
	} else {
		id = parsed.String()
	}
	err = executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, "INSERT INTO users (id,username) VALUES (?,?)", id, username); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO user_device (provider,id,user_id) VALUES (?,?,?)", provider, identity, id)
		return e
	})
	if err == nil {
		return id, username, true, nil
	}
	if mysqlErrorNumber(err, 1062) {
		existingID, existingName, e := lookup()
		if e == nil || status.Code(e) == codes.PermissionDenied {
			return existingID, existingName, false, e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return "", "", false, status.Error(codes.Internal, "Error finding user account.")
		}
		return "", "", false, status.Error(codes.AlreadyExists, "User ID, username or identity is already in use.")
	}
	return "", "", false, status.Error(codes.Internal, "Error finding or creating user account.")
}

func linkRegisteredProviderMySQL(ctx context.Context, db *sql.DB, user uuid.UUID, provider, identity string) error {
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		var owner, storedProvider string
		if e := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id=? FOR UPDATE", user.String()).Scan(&owner); e != nil {
			return e
		}
		e := tx.QueryRowContext(ctx, "SELECT user_id,provider FROM user_device WHERE id=?", identity).Scan(&owner, &storedProvider)
		if e == nil {
			if owner != user.String() || storedProvider != provider {
				return status.Error(codes.AlreadyExists, "Provider identity is already in use.")
			}
		} else if errors.Is(e, sql.ErrNoRows) {
			if _, e = tx.ExecContext(ctx, "INSERT INTO user_device (provider,id,user_id) VALUES (?,?,?)", provider, identity, user.String()); e != nil {
				return e
			}
		} else {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE users SET update_time=CURRENT_TIMESTAMP(6) WHERE id=?", user.String())
		return e
	})
	if err == nil || status.Code(err) == codes.AlreadyExists {
		return err
	}
	if mysqlErrorNumber(err, 1062) {
		return status.Error(codes.AlreadyExists, "Provider identity is already in use.")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return status.Error(codes.NotFound, "User account not found.")
	}
	return status.Error(codes.Internal, "Error linking provider identity.")
}

func unlinkRegisteredProviderMySQL(ctx context.Context, db *sql.DB, user uuid.UUID, provider string) error {
	denied := status.Error(codes.PermissionDenied, "Cannot unlink last account identifier. Check profile exists and is not last link.")
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		var usable bool
		e := tx.QueryRowContext(ctx, `SELECT (custom_id IS NOT NULL OR apple_id IS NOT NULL OR facebook_id IS NOT NULL OR facebook_instant_game_id IS NOT NULL OR google_id IS NOT NULL OR gamecenter_id IS NOT NULL OR steam_id IS NOT NULL OR (email IS NOT NULL AND password IS NOT NULL)) FROM users WHERE id=? FOR UPDATE`, user.String()).Scan(&usable)
		if errors.Is(e, sql.ErrNoRows) {
			return denied
		}
		if e != nil {
			return e
		}
		var others int
		if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_device WHERE user_id=? AND provider<>?", user.String(), provider).Scan(&others); e != nil {
			return e
		}
		if !usable && others == 0 {
			return denied
		}
		result, e := tx.ExecContext(ctx, "DELETE FROM user_device WHERE user_id=? AND provider=?", user.String(), provider)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			return denied
		}
		_, e = tx.ExecContext(ctx, "UPDATE users SET update_time=CURRENT_TIMESTAMP(6) WHERE id=?", user.String())
		return e
	})
	if err == nil || status.Code(err) == codes.PermissionDenied {
		return err
	}
	return status.Error(codes.Internal, "Error unlinking provider identity.")
}

// Registry names are trusted module configuration, but invalid names cannot be
// stored or confused with the empty namespace reserved for device identities.
func validateRegisteredProviderName(name string) error {
	if name == "" || len(name) > 128 || invalidCharsRegex.MatchString(name) || strings.Contains(name, "|") {
		return status.Error(codes.InvalidArgument, "Invalid authentication provider name.")
	}
	return nil
}
