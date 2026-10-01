// Copyright 2019 The Nakama Authors
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
	"crypto"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dgryski/dgoogauth"
	uuid "github.com/gofrs/uuid/v5"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/heroiclabs/nakama/v3/console/acl"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	passwordMinLength = 12
	passwordMaxLength = 128
)

type ConsoleTokenClaims struct {
	Revision  int64  `json:"rev,omitempty"`
	ID        string `json:"id,omitempty"`
	Username  string `json:"usn,omitempty"`
	Email     string `json:"ema,omitempty"`
	Acl       string `json:"acl,omitempty"`
	ExpiresAt int64  `json:"exp,omitempty"`
	Cookie    string `json:"cki,omitempty"`
}

func (s *ConsoleTokenClaims) GetExpirationTime() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(s.ExpiresAt, 0)), nil
}
func (s *ConsoleTokenClaims) GetNotBefore() (*jwt.NumericDate, error) {
	return nil, nil
}
func (s *ConsoleTokenClaims) GetIssuedAt() (*jwt.NumericDate, error) {
	return nil, nil
}
func (s *ConsoleTokenClaims) GetAudience() (jwt.ClaimStrings, error) {
	return []string{}, nil
}
func (s *ConsoleTokenClaims) GetIssuer() (string, error) {
	return "", nil
}
func (s *ConsoleTokenClaims) GetSubject() (string, error) {
	return "", nil
}

func parseConsoleToken(hmacSecretByte []byte, tokenString string) (id, username, email string, userAcl acl.Permission, exp int64, ok bool, err error) {
	token, err := jwt.ParseWithClaims(tokenString, &ConsoleTokenClaims{}, func(token *jwt.Token) (interface{}, error) {
		return hmacSecretByte, nil
	}, jwt.WithExpirationRequired(), jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return
	}
	claims, ok := token.Claims.(*ConsoleTokenClaims)
	if !ok || !token.Valid {
		return
	}
	aclBytes, err := base64.RawURLEncoding.DecodeString(claims.Acl)
	if err != nil {
		return "", "", "", acl.None(), 0, false, fmt.Errorf("failed to base64 decode ACL: %w", err)
	}
	return claims.ID, claims.Username, claims.Email, acl.NewFromBytes(aclBytes), claims.ExpiresAt, true, nil
}

func (s *ConsoleServer) Authenticate(ctx context.Context, in *console.AuthenticateRequest) (*console.ConsoleSession, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	ip, _ := extractClientAddressFromContext(logger, s.config, ctx)
	if !s.loginAttemptCache.Allow(in.Username, ip) {
		return nil, status.Error(codes.ResourceExhausted, "Try again later.")
	}

	var email string
	var userAcl acl.Permission
	var uname string
	var userId uuid.UUID
	var mfaCode *wrapperspb.StringValue
	var mfaRequired, mfaEnabled bool
	var sessionRevision int64
	transaction := func(tx *sql.Tx) error {
		var mfaSecret, mfaRecoveryCodes []byte
		switch in.Username {
		case s.config.GetConsole().Username:
			if in.Password == s.config.GetConsole().Password {
				userAcl = acl.Admin()
				uname = in.Username
				userId = uuid.Nil

				hashedPassword, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcryptHashCost)
				if err != nil {
					logger.Error("failed to hash admin console user password", zap.Error(err))
					return status.Error(codes.Internal, "failed to create admin console user")
				}

				aclJson, err := userAcl.ToJson()
				if err != nil {
					logger.Error("failed to serialize admin console user acl", zap.Error(err))
					return status.Error(codes.Internal, "failed to create admin console user")
				}

				// Create console root user to keep track of mfa configs.
				{
					err = consoleRootMySQL(ctx, tx, uname, in.Password, hashedPassword, aclJson, s.config.GetMFA().AdminAccountOn, &mfaSecret, &mfaRecoveryCodes, &mfaRequired)
				}

				if err != nil {
					logger.Error("failed to create admin console user", zap.Error(err))
					return status.Error(codes.Internal, "Internal error")
				}
			} else {
				if lockout, until := s.loginAttemptCache.Add(s.config.GetConsole().Username, ip); lockout != LockoutTypeNone {
					switch lockout {
					case LockoutTypeAccount:
						logger.Info(fmt.Sprintf("Console admin account locked until %v.", until))
					case LockoutTypeIp:
						logger.Info(fmt.Sprintf("Console admin IP locked until %v.", until))
					}
				}
				return status.Error(codes.Unauthenticated, "Invalid credentials.")
			}
		default:
			var err error
			userId, uname, email, userAcl, mfaRequired, mfaSecret, mfaRecoveryCodes, err = s.lookupConsoleUser(ctx, logger, tx, in.Username, in.Password, ip)
			if err != nil {
				return err
			}
		}

		if userAcl.IsNone() {
			return status.Error(codes.Unauthenticated, "Invalid credentials.")
		}

		s.loginAttemptCache.Reset(uname)

		mfaEnabled = mfaSecret != nil

		if mfaEnabled {
			if strings.TrimSpace(in.Mfa) == "" {
				return status.Error(codes.PermissionDenied, "A MFA code is required.")
			}
			recoveryCodesStr, err := decrypt(mfaRecoveryCodes, []byte(s.config.GetMFA().StorageEncryptionKey))
			if err != nil {
				logger.Error("Failed to decipher the MFA recovery codes for the user.", zap.Error(err))
				return status.Error(codes.Internal, "Failed to decipher the MFA recovery codes for the user.")
			}

			recoveryCodesStrArr := strings.Split(string(recoveryCodesStr), ",")
			recoveryCodes := make([]int, len(recoveryCodesStrArr))
			for i, codeStr := range recoveryCodesStrArr {
				recoveryCodes[i], err = strconv.Atoi(codeStr)
				if err != nil {
					logger.Error("Failed to parse a MFA recovery codes as an integer.", zap.Error(err))
					return status.Error(codes.Internal, "Failed to parse a MFA recovery codes as an integer.")
				}
			}

			mfaSecret, err := decrypt(mfaSecret, []byte(s.config.GetMFA().StorageEncryptionKey))
			if err != nil {
				logger.Error("Failed to decipher the MFA secret for the user.", zap.Error(err))
				return status.Error(codes.Internal, "Failed to decipher the MFA secret for the user.")
			}

			optConfig := &dgoogauth.OTPConfig{
				Secret:       string(mfaSecret),
				WindowSize:   MFAWindowSize,
				UTC:          true,
				ScratchCodes: recoveryCodes,
			}

			ok, err := optConfig.Authenticate(in.Mfa)
			if err != nil {
				return status.Error(codes.InvalidArgument, "The MFA code used is incorrectly formatted.")
			}
			if !ok {
				return status.Error(codes.Unauthenticated, "The MFA code is invalid.")
			}

			if len(optConfig.ScratchCodes) != len(recoveryCodesStrArr) {
				// The user used a recovery code so the new list must be persisted.
				recoveryCodesStrArray := make([]string, len(optConfig.ScratchCodes))
				for i, code := range optConfig.ScratchCodes {
					recoveryCodesStrArray[i] = strconv.Itoa(code)
				}

				updatedRecoveryCodes, err := encrypt([]byte(strings.Join(recoveryCodesStrArray, ",")), []byte(s.config.GetMFA().StorageEncryptionKey))
				if err != nil {
					logger.Error("Failed to decipher the MFA secret for the user.", zap.Error(err))
					return status.Error(codes.Internal, "Failed to decipher the MFA secret for the user.")
				}

				query := "UPDATE console_user SET mfa_recovery_codes = ? WHERE id = ?"
				if _, err = tx.ExecContext(ctx, query, updatedRecoveryCodes, userId); err != nil {
					if errors.Is(err, context.Canceled) {
						return err
					}
					logger.Error("Failed to update the recovery codes for the user.", zap.Error(err))
					return status.Error(codes.Internal, "Failed to update the recovery codes for the user.")
				}
			}
		} else if !mfaEnabled {
			consoleConfig := s.config.GetConsole()
			tokenTime := time.Now()
			mfaSecret, err := generateMFASecret()
			if err != nil {
				logger.Error("Failed to generate the MFA secret to re-configure the MFA mechanism.", zap.Error(err))
				return status.Error(codes.Internal, "Failed to generate the MFA secret to re-configure the MFA mechanism.")
			}

			var revision int64
			{
				var updated time.Time
				query := "SELECT update_time FROM console_user WHERE id = ?"
				if err := tx.QueryRowContext(ctx, query, userId).Scan(&updated); err != nil {
					return err
				}
				revision = updated.UnixMicro()
			}
			mfaToken, err := generateJWTToken(
				consoleConfig.SigningKey,
				&UserMFASetupToken{
					UserID:      userId.String(),
					Revision:    revision,
					UserEmail:   email,
					ExpiryTime:  tokenTime.Add(time.Duration(consoleConfig.TokenExpirySec) * time.Second).Unix(),
					CreateTime:  tokenTime.Unix(),
					MFASecret:   mfaSecret,
					MFAUrl:      generateMFAUrl(mfaSecret, uname),
					MFARequired: mfaRequired,
				},
			)
			if err != nil {
				logger.Error("Failed generate one-time code to re-configure the user's MFA mechanism.", zap.Error(err))
				return status.Errorf(codes.Internal, "Failed generate one-time code to re-configure the user's MFA mechanism.")
			}

			mfaCode = &wrapperspb.StringValue{Value: mfaToken}
		}

		{
			var updated time.Time
			query := "SELECT update_time FROM console_user WHERE id = ?"
			if err := tx.QueryRowContext(ctx, query, userId).Scan(&updated); err != nil {
				return err
			}
			sessionRevision = updated.UnixMicro()
		}

		return nil
	}

	if err := ExecuteInTx(ctx, s.db, transaction); err != nil {
		return nil, err
	}

	if mfaRequired && !mfaEnabled {
		return &console.ConsoleSession{MfaCode: mfaCode}, nil
	}

	// MFA not enabled, regular login
	exp := time.Now().UTC().Add(time.Duration(s.config.GetConsole().TokenExpirySec) * time.Second).Unix()

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, &ConsoleTokenClaims{
		ExpiresAt: exp,
		Revision:  sessionRevision,
		ID:        userId.String(),
		Username:  uname,
		Email:     email,
		Acl:       userAcl.String(),
		Cookie:    s.cookie,
	})
	key := []byte(s.config.GetConsole().SigningKey)
	signedToken, err := token.SignedString(key)
	if err != nil {
		logger.Error("Failed to sign the token", zap.Error(err))
		return nil, status.Error(codes.Internal, "Failed to sign the token.")
	}

	s.consoleSessionCache.Add(userId, exp, signedToken, 0, "")

	return &console.ConsoleSession{Token: signedToken}, nil
}

func (s *ConsoleServer) AuthenticateLogout(ctx context.Context, in *console.AuthenticateLogoutRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	token, err := jwt.Parse(in.Token, func(token *jwt.Token) (interface{}, error) {
		if s, ok := token.Method.(*jwt.SigningMethodHMAC); !ok || s.Hash != crypto.SHA256 {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(s.config.GetConsole().SigningKey), nil
	})
	if err != nil {
		logger.Error("Failed to parse the session token.", zap.Error(err))
	}
	id, _, _, _, exp, ok, err := parseConsoleToken([]byte(s.config.GetConsole().SigningKey), in.Token)
	if err != nil {
		logger.Error("Failed to parse token console jwt token.", zap.Error(err))
		return &emptypb.Empty{}, nil
	}
	if !ok || !token.Valid {
		logger.Error("Invalid token.", zap.Error(err))
	}
	idUuid, err := uuid.FromString(id)
	if id != "" && err == nil {
		s.consoleSessionCache.Remove(idUuid, exp, in.Token, 0, "")
	}

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) AuthenticateMFASetup(ctx context.Context, in *console.AuthenticateMFASetupRequest) (*console.AuthenticateMFASetupResponse, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	var claims UserMFASetupToken
	if err := parseJWTToken(s.config.GetConsole().SigningKey, in.GetMfa(), &claims); err != nil {
		logger.Warn("Failed to parse the JTW provided as code.", zap.Error(err))
		return nil, status.Errorf(codes.Unauthenticated, "The code provided is invalid.")
	}

	userId := claims.UserID

	logger = logger.With(zap.String("user_id", userId))

	mfaConfig := &dgoogauth.OTPConfig{
		Secret:     claims.MFASecret,
		WindowSize: MFAWindowSize,
		UTC:        true,
	}

	ok, err := mfaConfig.Authenticate(in.Code)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "The MFA code used is incorrectly formatted.")
	}
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "The MFA code is invalid.")
	}

	dbMFASecret, err := encrypt([]byte(mfaConfig.Secret), []byte(s.config.GetMFA().StorageEncryptionKey))
	if err != nil {
		logger.Error("Failed to cipher the MFA secret.", zap.Error(err))
		return nil, status.Error(codes.Internal, "Failed to encrypt the MFA secret.s")
	}

	recoveryCodes, err := generateRecoveryCodes()
	if err != nil {
		logger.Error("Failed to generate the MFA Recovery Codes.", zap.Error(err))
		return nil, status.Error(codes.Internal, "Failed to generate the MFA Recovery Codes.")
	}

	dbRecoveryCodes, err := encrypt([]byte(strings.Join(recoveryCodes, ",")), []byte(s.config.GetMFA().StorageEncryptionKey))
	if err != nil {
		logger.Error("Failed to cipher the MFA Recovery Codes.", zap.Error(err))
		return nil, status.Error(codes.Internal, "Failed to encrypt the MFA Recovery Codes.")
	}

	query := "UPDATE console_user SET mfa_secret = ?, mfa_recovery_codes = ?, update_time = GREATEST(CURRENT_TIMESTAMP(6), update_time + INTERVAL 1 MICROSECOND) WHERE id = ? AND mfa_secret IS NULL AND update_time = ?"
	var res sql.Result
	res, err = s.db.ExecContext(ctx, query, dbMFASecret, dbRecoveryCodes, userId, time.UnixMicro(claims.Revision).UTC())
	if err == nil {
		var count int64
		count, err = res.RowsAffected()
		if err == nil && count != 1 {
			err = sql.ErrNoRows
		}
	}

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			logger.Warn("The token provided was created before the last update of the user data.")
			return nil, status.Error(codes.Unauthenticated, "The token is outdated.")
		}

		logger.Error("Failed to update the user record with the new MFA secret and Recovery Codes.", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "Failed to update the user record with the new MFA secret and Recovery Codes.")
	}

	s.consoleSessionCache.RemoveAll(uuid.FromStringOrNil(userId))
	return &console.AuthenticateMFASetupResponse{RecoveryCodes: recoveryCodes}, nil
}

func (s *ConsoleServer) AuthenticatePasswordChange(ctx context.Context, in *console.AuthenticatePasswordChangeRequest) (*emptypb.Empty, error) {
	if strings.TrimSpace(in.Password) == "" {
		return nil, status.Error(codes.InvalidArgument, "Password must be set.")
	}

	if len(in.Password) < passwordMinLength || len(in.Password) > passwordMaxLength {
		return nil, status.Error(codes.InvalidArgument, "Password must be between 12 and 128 characters long.")
	}

	var claims UserInvitationClaims
	if err := parseJWTToken(s.config.GetConsole().SigningKey, in.Code, &claims); err != nil {
		s.logger.Warn("Failed to parse the JWT provided as code.", zap.Error(err))
		return nil, status.Errorf(codes.Unauthenticated, "The code provided is invalid.")
	}

	logger := s.logger.With(zap.String("user_id", claims.Id))

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcryptHashCost)
	if err != nil {
		logger.Error("Failed to hash the password for the user.", zap.Error(err))
		return nil, status.Error(codes.Internal, "Failed to hash the password for the user.")
	}

	query := ""

	args := []any{hashedPassword, claims.Id, time.UnixMicro(claims.Revision).UTC()}
	{
		query = "UPDATE console_user SET password = ?, update_time = GREATEST(CURRENT_TIMESTAMP(6), update_time + INTERVAL 1 MICROSECOND) WHERE id = ? AND update_time = ?"
		args[2] = time.UnixMicro(claims.Revision).UTC()
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		logger.Error("Failed to update the console user record with a new password.", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "Failed to update the console user record with a new password.")
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		logger.Error("Failed to get the number of rows affected.", zap.Error(err))
		return nil, status.Error(codes.Internal, "Failed to update user password.")
	}

	if rowsAffected < 1 {
		s.logger.Warn("The token provided was created before the last update of the user data.")
		return nil, status.Error(codes.Unauthenticated, "The token is outdated. Please ask your administrator for a new password reset.")
	}

	s.consoleSessionCache.RemoveAll(uuid.FromStringOrNil(claims.Id))
	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) lookupConsoleUser(ctx context.Context, logger *zap.Logger, tx *sql.Tx, unameOrEmail, password, ip string) (id uuid.UUID, uname string, email string, role acl.Permission, mfaRequired bool, mfaSecret, mfaRecoveryCodes []byte, err error) {
	role = acl.None()
	var permissionBytes []byte

	var dbPassword []byte
	var dbDisableTime sql.NullTime
	var row Scannable
	{
		// Hold the user lock through recovery-code consumption and authentication.
		row = tx.QueryRowContext(ctx, "SELECT id, username, email, acl, password, mfa_required, mfa_secret, mfa_recovery_codes, disable_time FROM console_user WHERE username = ? OR email = ? FOR UPDATE", unameOrEmail, unameOrEmail)
	}

	err = row.Scan(&id, &uname, &email, &permissionBytes, &dbPassword, &mfaRequired, &mfaSecret, &mfaRecoveryCodes, &dbDisableTime)
	if err != nil {
		if err == sql.ErrNoRows {
			if lockout, until := s.loginAttemptCache.Add("", ip); lockout == LockoutTypeIp {
				logger.Info(fmt.Sprintf("Console user IP locked until %v.", until))
			}
			err = status.Error(codes.Unauthenticated, "Invalid credentials.")
		}
		// Call hash function to help obfuscate response time when user does not exist.
		var dummyHash = []byte("$2y$10$x8B0hPVxYGDq7bZiYC9jcuwA0B9m4J6vYITYIv0nf.IfYuM1kGI3W")
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return
	}

	if dbPassword == nil {
		// The user has not yet set his password.
		err = status.Error(codes.Unauthenticated, "Invalid credentials.")
		return
	}

	// Check lockout again as the login attempt may have been through email.
	if !s.loginAttemptCache.Allow(uname, ip) {
		err = status.Error(codes.ResourceExhausted, "Try again later.")
		return
	}

	// Check if it's disabled.
	if dbDisableTime.Valid && dbDisableTime.Time.Unix() != 0 {
		logger.Info("Console user account is disabled.", zap.String("username", unameOrEmail))
		err = status.Error(codes.PermissionDenied, "Invalid credentials.")
		return
	}

	// Check password
	err = bcrypt.CompareHashAndPassword(dbPassword, []byte(password))
	if err != nil {
		if lockout, until := s.loginAttemptCache.Add(uname, ip); lockout != LockoutTypeNone {
			switch lockout {
			case LockoutTypeAccount:
				logger.Info(fmt.Sprintf("Console user account locked until %v.", until))
			case LockoutTypeIp:
				logger.Info(fmt.Sprintf("Console user IP locked until %v.", until))
			}
		}
		err = status.Error(codes.Unauthenticated, "Invalid credentials.")
		return
	}

	role, err = acl.NewFromJson(string(permissionBytes))
	if err != nil {
		logger.Error("Failed to parse json ACL for console user.", zap.String("username", unameOrEmail), zap.Error(err))
		err = status.Error(codes.Internal, "Error looking up console user.")
	}

	return
}
