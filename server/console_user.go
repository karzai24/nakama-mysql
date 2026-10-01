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
	"bytes"
	"context"

	"database/sql"
	"encoding/json"

	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/heroiclabs/nakama/v3/console"
	"github.com/heroiclabs/nakama/v3/console/acl"

	"go.uber.org/zap"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var usernameRegex = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9._].*[a-zA-Z0-9]$")

type UserInvitationClaims struct {
	Revision  int64  `json:"rev,omitempty"`
	Id        string `json:"uid,omitempty"`
	Username  string `json:"usn,omitempty"`
	Email     string `json:"ema,omitempty"`
	ExpiresAt int64  `json:"exp,omitempty"`
	IssuedAt  int64  `json:"iat,omitempty"`
}

func (s *UserInvitationClaims) GetExpirationTime() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(s.ExpiresAt, 0)), nil
}
func (s *UserInvitationClaims) GetNotBefore() (*jwt.NumericDate, error) {
	return nil, nil
}
func (s *UserInvitationClaims) GetIssuedAt() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(s.IssuedAt, 0)), nil
}
func (s *UserInvitationClaims) GetAudience() (jwt.ClaimStrings, error) {
	return []string{}, nil
}
func (s *UserInvitationClaims) GetIssuer() (string, error) {
	return "", nil
}
func (s *UserInvitationClaims) GetSubject() (string, error) {
	return s.Id, nil
}

func (s *ConsoleServer) AddUser(ctx context.Context, in *console.AddUserRequest) (*console.AddUserResponse, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	uname := ctx.Value(ctxConsoleUsernameKey{}).(string)
	if uname == in.Username {
		return nil, status.Error(codes.FailedPrecondition, "Cannot change own configuration")
	}

	if in.Username == "" {
		return nil, status.Error(codes.InvalidArgument, "Username is required")
	} else if len(in.Username) < 3 || len(in.Username) > 20 || !usernameRegex.MatchString(in.Username) {
		return nil, status.Error(codes.InvalidArgument, "Username must be 3-20 long sequence of alphanumeric characters _ or . and cannot start and end with _ or .")
	}
	in.Username = strings.ToLower(in.Username)

	if in.Username == "admin" || in.Username == s.config.GetConsole().Username {
		return nil, status.Error(codes.InvalidArgument, "Username cannot be the console configured username")
	}

	if in.Email == "" {
		return nil, status.Error(codes.InvalidArgument, "Email is required")
	} else if len(in.Email) < 3 || len(in.Email) > 254 || !emailRegex.MatchString(in.Email) || invalidCharsRegex.MatchString(in.Email) {
		return nil, status.Error(codes.InvalidArgument, "Not a valid email address")
	}
	in.Email = strings.ToLower(in.Email)

	if in.NewsletterSubscription {
		inviterUsername := ctx.Value(ctxConsoleUsernameKey{}).(string)
		inviterEmail := ctx.Value(ctxConsoleEmailKey{}).(string)
		payload := map[string]interface{}{
			"email":            in.Email,
			"username":         in.Username,
			"cookie":           s.cookie,
			"inviter_username": inviterUsername,
			"inviter_email":    inviterEmail,
			"newsletter":       in.NewsletterSubscription,
		}

		if payloadJson, err := json.Marshal(payload); err != nil {
			logger.Debug("Failed to create newsletter request payload.", zap.Error(err))
		} else {
			if req, err := http.NewRequest(http.MethodPost, "https://cloud.heroiclabs.com/v1/nakama-newsletter/subscribe", bytes.NewBuffer(payloadJson)); err != nil {
				logger.Debug("Failed to create newsletter request.", zap.Error(err))
			} else {
				req.Header.Set("Content-Type", "application/json")
				if resp, err := s.httpClient.Do(req); err != nil {
					logger.Debug("Failed to add newsletter subscription.", zap.Error(err))
				} else {
					resp.Body.Close()
					logger.Debug("Added newsletter subscription.", zap.Int("status", resp.StatusCode))
				}
			}
		}

	}

	user, err := s.dbInsertConsoleUser(ctx, logger, in)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		} else {
			logger.Error("failed to insert console user", zap.Error(err), zap.String("username", in.Username), zap.String("email", in.Email))
			return nil, status.Error(codes.Internal, "Internal Server Error")
		}
	}

	token, err := generateJWTToken(
		s.config.GetConsole().SigningKey,
		&UserInvitationClaims{
			Id:        user.Id,
			Username:  user.Username,
			Email:     user.Email,
			ExpiresAt: user.UpdateTime.AsTime().Add(time.Duration(s.config.GetConsole().TokenExpirySec) * time.Second).Unix(),
			IssuedAt:  user.UpdateTime.AsTime().UTC().Unix(),
			Revision:  user.UpdateTime.AsTime().UnixMicro(),
		},
	)
	if err != nil {
		logger.Error("failed to generate console user token", zap.Error(err), zap.String("username", in.Username), zap.String("email", in.Email))
		return nil, status.Error(codes.Internal, "Internal Server Error")
	}

	return &console.AddUserResponse{User: user, Token: token}, nil
}

func (s *ConsoleServer) dbInsertConsoleUser(ctx context.Context, logger *zap.Logger, in *console.AddUserRequest) (*console.User, error) {

	return s.insertConsoleUserMySQL(ctx, in)

}

func (s *ConsoleServer) GetUser(ctx context.Context, in *console.Username) (*console.User, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	users, err := s.dbListConsoleUsers(ctx, []string{in.Username})
	if err != nil {
		logger.Error("failed to list console users", zap.Error(err))
		return nil, err
	}

	if len(users) == 0 {
		return nil, status.Error(codes.NotFound, "User not found")
	}

	return users[0], nil
}

func (s *ConsoleServer) UpdateUser(ctx context.Context, in *console.UpdateUserRequest) (*console.User, error) {
	creatorRole := ctx.Value(ctxConsoleUserAclKey{}).(acl.Permission)
	uname := ctx.Value(ctxConsoleUsernameKey{}).(string)

	if in.Username == uname {
		return nil, status.Error(codes.FailedPrecondition, "Cannot change own configuration")
	}

	role := acl.New(in.Acl)
	if !creatorRole.HasAccess(role) {
		return nil, status.Error(codes.InvalidArgument, "Cannot create users with more permissions that the one in session.")
	}

	var update *console.User

	if err := ExecuteInTx(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		{
			update, err = updateConsoleUserMySQL(ctx, tx, in)
		}

		if err != nil {
			return err
		}

		return nil
	}); err != nil {
		return nil, err
	}

	s.consoleSessionCache.RemoveAll(uuid.Must(uuid.FromString(update.Id)))

	return update, nil
}

func (s *ConsoleServer) ResetUserPassword(ctx context.Context, in *console.Username) (*console.ResetUserResponse, error) {

	return s.resetConsolePasswordMySQL(ctx, in.Username)

}

func (s *ConsoleServer) DeleteUser(ctx context.Context, in *console.Username) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	uname := ctx.Value(ctxConsoleUsernameKey{}).(string)

	if in.Username == uname {
		return nil, status.Error(codes.FailedPrecondition, "Cannot delete own user")
	}

	deleted, id, err := s.dbDeleteConsoleUser(ctx, in.Username)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		logger.Error("failed to delete console user", zap.Error(err), zap.String("username", in.Username))
		return nil, status.Error(codes.Internal, "Internal Server Error")
	} else if !deleted {
		return nil, status.Error(codes.InvalidArgument, "User not found")
	}
	s.consoleSessionCache.RemoveAll(id)

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) ListUsers(ctx context.Context, in *emptypb.Empty) (*console.UserList, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	users, err := s.dbListConsoleUsers(ctx, nil)
	if err != nil {
		logger.Error("failed to list console users", zap.Error(err))
		return nil, status.Error(codes.Internal, "Internal Server Error")
	}
	return &console.UserList{Users: users}, nil
}

func (s *ConsoleServer) dbListConsoleUsers(ctx context.Context, usernames []string) ([]*console.User, error) {
	result := make([]*console.User, 0, 10)
	query := ""
	params := []any{uuid.Nil}
	if len(usernames) > 0 {

		params = append(params, usernames)
	}
	{
		query = "SELECT id, username, email, acl, mfa_required, mfa_secret IS NOT NULL, create_time, update_time FROM console_user WHERE id != ?"
		params = []any{uuid.Nil}
		if len(usernames) > 0 {
			query += " AND username IN (" + mysqlMarks(len(usernames)) + ")"
			for _, name := range usernames {
				params = append(params, name)
			}
		}
		query += " ORDER BY username"
	}
	rows, err := s.db.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		user := &console.User{}
		var createTime, updateTime time.Time
		var aclBytes []byte
		if err := rows.Scan(&user.Id, &user.Username, &user.Email, &aclBytes, &user.MfaRequired, &user.MfaEnabled, &createTime, &updateTime); err != nil {
			_ = rows.Close()
			return nil, err
		}
		user.CreateTime = timestamppb.New(createTime)
		user.UpdateTime = timestamppb.New(updateTime)
		userAcl, err := acl.NewFromJson(string(aclBytes))
		if err != nil {
			return nil, err
		}
		user.Acl = userAcl.ACL()
		result = append(result, user)
	}
	_ = rows.Close()

	return result, rows.Err()
}

func (s *ConsoleServer) dbDeleteConsoleUser(ctx context.Context, username string) (bool, uuid.UUID, error) {

	return deleteConsoleUserMySQL(ctx, s.db, username)

}

func (s *ConsoleServer) RequireUserMfa(ctx context.Context, in *console.RequireUserMfaRequest) (*emptypb.Empty, error) {

	return s.consoleMFAMySQL(ctx, in.Username, &in.Required)

}

func (s *ConsoleServer) ResetUserMfa(ctx context.Context, in *console.ResetUserMfaRequest) (*emptypb.Empty, error) {

	return s.consoleMFAMySQL(ctx, in.Username, nil)

}

type UserMFASetupToken struct {
	Revision    int64  `json:"rev,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	UserEmail   string `json:"user_email,omitempty"`
	ExpiryTime  int64  `json:"exp,omitempty"`
	CreateTime  int64  `json:"crt,omitempty"`
	MFASecret   string `json:"secret,omitempty"`
	MFAUrl      string `json:"mfa_url,omitempty"`
	MFARequired bool   `json:"mfa_required,omitempty"`
}

func (s *UserMFASetupToken) GetExpirationTime() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(s.ExpiryTime, 0)), nil
}
func (s *UserMFASetupToken) GetNotBefore() (*jwt.NumericDate, error) {
	return nil, nil
}
func (s *UserMFASetupToken) GetIssuedAt() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(s.CreateTime, 0)), nil
}
func (s *UserMFASetupToken) GetAudience() (jwt.ClaimStrings, error) {
	return []string{}, nil
}
func (s *UserMFASetupToken) GetIssuer() (string, error) {
	return "", nil
}
func (s *UserMFASetupToken) GetSubject() (string, error) {
	return "", nil
}
