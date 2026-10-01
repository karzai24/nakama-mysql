// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/heroiclabs/nakama/v3/console/acl"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func consoleRootMySQL(ctx context.Context, tx *sql.Tx, name, plainPassword string, password []byte, permissions string, required bool, secret, recovery *[]byte, mfaRequired *bool) error {
	// A collision on email/username must never update an unrelated administrator.
	_, err := tx.ExecContext(ctx, "INSERT INTO console_user (id, username, email, password, acl, mfa_required) VALUES (?,?,?,?,?,?)", uuid.Nil, name, "admin@nakama", password, permissions, required)
	if err != nil && !mysqlErrorNumber(err, 1062) {
		return err
	}

	var storedName, storedACL string
	var storedPassword []byte
	var storedRequired bool
	if err = tx.QueryRowContext(ctx, "SELECT username,password,acl,mfa_required FROM console_user WHERE id = ? FOR UPDATE", uuid.Nil).Scan(&storedName, &storedPassword, &storedACL, &storedRequired); err != nil {
		return err
	}
	// Preserve the revision on ordinary root logins; only configuration changes
	// invalidate existing sessions. Password hashes have random salts.
	existingPermissions, e := acl.NewFromJson(storedACL)
	if e != nil {
		return e
	}
	if storedName != name || storedRequired != required || !existingPermissions.IsAdmin() || bcrypt.CompareHashAndPassword(storedPassword, []byte(plainPassword)) != nil {
		if _, err = tx.ExecContext(ctx, "UPDATE console_user SET username = ?, password = ?, acl = ?, mfa_required = ?, update_time = GREATEST(CURRENT_TIMESTAMP(6), update_time + INTERVAL 1 MICROSECOND) WHERE id = ?", name, password, permissions, required, uuid.Nil); err != nil {
			return err
		}
	}

	return tx.QueryRowContext(ctx, "SELECT mfa_secret, mfa_recovery_codes, mfa_required FROM console_user WHERE id = ?", uuid.Nil).Scan(secret, recovery, mfaRequired)
}

const consoleUserColumnsMySQL = "id, username, email, acl, mfa_required, mfa_secret IS NOT NULL, create_time, update_time"

func scanConsoleUserMySQL(row Scannable) (*console.User, error) {
	u := &console.User{}
	var permissions string
	var created, updated time.Time
	if err := row.Scan(&u.Id, &u.Username, &u.Email, &permissions, &u.MfaRequired, &u.MfaEnabled, &created, &updated); err != nil {
		return nil, err
	}
	p, err := acl.NewFromJson(permissions)
	if err != nil {
		return nil, err
	}
	u.Acl = p.ACL()
	u.CreateTime = timestamppb.New(created)
	u.UpdateTime = timestamppb.New(updated)
	return u, nil
}
func (s *ConsoleServer) insertConsoleUserMySQL(ctx context.Context, in *console.AddUserRequest) (*console.User, error) {
	p := acl.New(in.Acl)
	if p.IsNone() {
		return nil, status.Error(codes.InvalidArgument, "User must have at least some permissions.")
	}
	if caller, ok := ctx.Value(ctxConsoleUserAclKey{}).(acl.Permission); ok && !caller.HasAccess(p) {
		return nil, status.Error(codes.PermissionDenied, "Cannot grant permissions you do not have.")
	}
	permissions, err := p.ToJson()
	if err != nil {
		return nil, err
	}
	var user *console.User
	err = executeInTxMySQL(ctx, s.db, func(tx *sql.Tx) error {
		id := uuid.Must(uuid.NewV4())
		_, err := tx.ExecContext(ctx, "INSERT INTO console_user (id,username,email,acl,mfa_required) VALUES (?,?,?,?,?)", id, in.Username, in.Email, permissions, in.MfaRequired)
		if mysqlErrorNumber(err, 1062) {
			// Match PostgreSQL's username conflict target; email collisions alone fail.
			var current string
			if e := tx.QueryRowContext(ctx, "SELECT id FROM console_user WHERE username = ? FOR UPDATE", in.Username).Scan(&current); e != nil {
				return status.Error(codes.FailedPrecondition, "Username or Email already exists")
			}
			if current == uuid.Nil.String() {
				return status.Error(codes.PermissionDenied, "Cannot replace the configured administrator.")
			}
			_, err = tx.ExecContext(ctx, "UPDATE console_user SET acl = ?, mfa_required = ?, update_time = GREATEST(CURRENT_TIMESTAMP(6), update_time + INTERVAL 1 MICROSECOND) WHERE id = ?", permissions, in.MfaRequired, current)
		}
		if err != nil {
			if mysqlErrorNumber(err, 1062) {
				return status.Error(codes.FailedPrecondition, "Username or Email already exists")
			}
			return err
		}
		user, err = scanConsoleUserMySQL(tx.QueryRowContext(ctx, "SELECT "+consoleUserColumnsMySQL+" FROM console_user WHERE username = ?", in.Username))
		return err
	})
	if err != nil {
		return nil, err
	}
	s.consoleSessionCache.RemoveAll(uuid.FromStringOrNil(user.Id))
	return user, nil
}
func updateConsoleUserMySQL(ctx context.Context, tx *sql.Tx, in *console.UpdateUserRequest) (*console.User, error) {
	p := acl.New(in.Acl)
	if p.IsNone() {
		return nil, status.Error(codes.InvalidArgument, "User must have at least some permissions.")
	}
	permissions, err := p.ToJson()
	if err != nil {
		return nil, err
	}
	u, err := scanConsoleUserMySQL(tx.QueryRowContext(ctx, "SELECT "+consoleUserColumnsMySQL+" FROM console_user WHERE username = ? FOR UPDATE", in.Username))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "User not found.")
	}
	if err != nil {
		return nil, err
	}
	if u.Id == uuid.Nil.String() {
		return nil, status.Error(codes.PermissionDenied, "Cannot change the configured administrator.")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE console_user SET acl = ?, update_time = GREATEST(CURRENT_TIMESTAMP(6), update_time + INTERVAL 1 MICROSECOND) WHERE id = ?", permissions, u.Id); err != nil {
		return nil, err
	}
	return scanConsoleUserMySQL(tx.QueryRowContext(ctx, "SELECT "+consoleUserColumnsMySQL+" FROM console_user WHERE id = ?", u.Id))
}
func (s *ConsoleServer) resetConsolePasswordMySQL(ctx context.Context, name string) (*console.ResetUserResponse, error) {
	password := make([]byte, 32)
	if _, err := rand.Read(password); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword(password, bcryptHashCost)
	if err != nil {
		return nil, err
	}
	var u *console.User
	err = executeInTxMySQL(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		u, err = scanConsoleUserMySQL(tx.QueryRowContext(ctx, "SELECT "+consoleUserColumnsMySQL+" FROM console_user WHERE username = ? FOR UPDATE", name))
		if err != nil {
			return err
		}
		if u.Id == uuid.Nil.String() {
			return status.Error(codes.PermissionDenied, "The configured administrator password is managed by server configuration.")
		}
		if _, err = tx.ExecContext(ctx, "UPDATE console_user SET password = ?, update_time = GREATEST(CURRENT_TIMESTAMP(6), update_time + INTERVAL 1 MICROSECOND) WHERE id = ?", hash, u.Id); err != nil {
			return err
		}
		u, err = scanConsoleUserMySQL(tx.QueryRowContext(ctx, "SELECT "+consoleUserColumnsMySQL+" FROM console_user WHERE id = ?", u.Id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "User not found.")
	}
	if err != nil {
		return nil, err
	}
	s.consoleSessionCache.RemoveAll(uuid.FromStringOrNil(u.Id))
	ts := u.UpdateTime.AsTime()
	token, err := generateJWTToken(s.config.GetConsole().SigningKey, &UserInvitationClaims{Id: u.Id, Username: u.Username, Email: u.Email, IssuedAt: ts.Unix(), Revision: ts.UnixMicro(), ExpiresAt: ts.Add(time.Duration(s.config.GetConsole().TokenExpirySec) * time.Second).Unix()})
	if err != nil {
		return nil, err
	}
	return &console.ResetUserResponse{Code: token}, nil
}
func deleteConsoleUserMySQL(ctx context.Context, db *sql.DB, name string) (bool, uuid.UUID, error) {
	var id uuid.UUID
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT id FROM console_user WHERE username = ? FOR UPDATE", name).Scan(&id); err != nil {
			return err
		}
		if id == uuid.Nil {
			return status.Error(codes.PermissionDenied, "Cannot delete the configured administrator.")
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM console_user WHERE id = ?", id)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, uuid.Nil, nil
	}
	return err == nil, id, err
}
func (s *ConsoleServer) consoleMFAMySQL(ctx context.Context, name string, required *bool) (*emptypb.Empty, error) {
	var id uuid.UUID
	err := executeInTxMySQL(ctx, s.db, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT id FROM console_user WHERE username = ? FOR UPDATE", name).Scan(&id); err != nil {
			return err
		}
		var err error
		if required == nil {
			_, err = tx.ExecContext(ctx, "UPDATE console_user SET mfa_secret = NULL, mfa_recovery_codes = NULL, update_time = GREATEST(CURRENT_TIMESTAMP(6), update_time + INTERVAL 1 MICROSECOND) WHERE id = ?", id)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE console_user SET mfa_required = ?, update_time = GREATEST(CURRENT_TIMESTAMP(6), update_time + INTERVAL 1 MICROSECOND) WHERE id = ?", *required, id)
		}
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "User not found.")
	}
	if err != nil {
		return nil, err
	}
	s.consoleSessionCache.RemoveAll(id)
	return &emptypb.Empty{}, nil
}

// Persistent revisions prevent deleted/demoted administrators retaining old ACLs,
// including changes within a single second and after the server restarts.
func consoleRevision(ctx context.Context, db *sql.DB, id uuid.UUID, token *jwt.Token) bool {
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return false
	}
	revision, ok := claims["rev"].(float64)
	if !ok || revision <= 0 {
		return false
	}
	var updated, disabled time.Time
	var required, enabled bool
	query := "SELECT update_time,disable_time,mfa_required,mfa_secret IS NOT NULL FROM console_user WHERE id = ?"
	if err := db.QueryRowContext(ctx, query, id).Scan(&updated, &disabled, &required, &enabled); err != nil {
		return false
	}
	return updated.UnixMicro() == int64(revision) && disabled.Unix() == 0 && (!required || enabled)
}
