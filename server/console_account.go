// Modified by nakama-mysql contributors: native MySQL account lifecycle integration.
// Copyright 2018 The Nakama Authors
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
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"errors"
	"html"
	"regexp"

	"strconv"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/console"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var validTrigramFilterRegex = regexp.MustCompile("^%?[^%]{3,}%?$")

type consoleAccountCursor struct {
	ID       uuid.UUID
	Username string
}

type consoleAccountNotesCursor struct {
	NoteID     uuid.UUID
	UserID     uuid.UUID
	CreateTime time.Time
}

func (s *ConsoleServer) BanAccount(ctx context.Context, in *console.AccountId) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	if userID == uuid.Nil {
		return nil, status.Error(codes.InvalidArgument, "Cannot ban the system user.")
	}

	if err := BanUsers(ctx, logger, s.db, s.config, s.sessionCache, s.sessionRegistry, s.tracker, []uuid.UUID{userID}); err != nil {
		// Error logged in the core function above.
		return nil, status.Error(codes.Internal, "An error occurred while trying to ban the user.")
	}

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) UnbanAccount(ctx context.Context, in *console.AccountId) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	if userID == uuid.Nil {
		return nil, status.Error(codes.InvalidArgument, "Cannot unban the system user.")
	}

	if err := UnbanUsers(ctx, logger, s.db, s.sessionCache, []uuid.UUID{userID}); err != nil {
		// Error logged in the core function above.
		return nil, status.Error(codes.Internal, "An error occurred while trying to unban the user.")
	}

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) DeleteAccount(ctx context.Context, in *console.AccountDeleteRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	if err = DeleteAccount(ctx, logger, s.db, s.config, s.leaderboardCache, s.leaderboardRankCache, s.sessionRegistry, s.sessionCache, s.tracker, userID, in.RecordDeletion != nil && in.RecordDeletion.Value, s.storageIndex); err != nil {
		// Error already logged in function above.
		return nil, status.Error(codes.Internal, "An error occurred while trying to delete the user.")
	}

	return &emptypb.Empty{}, nil
}

// Deprecated: replaced by DeleteAllData
func (s *ConsoleServer) DeleteAccounts(ctx context.Context, in *emptypb.Empty) (*emptypb.Empty, error) {

	return s.deleteAccountsMySQL(ctx)

}

func (s *ConsoleServer) DeleteFriend(ctx context.Context, in *console.DeleteFriendRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	if _, err := uuid.FromString(in.FriendId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid friend ID.")
	}

	users, err := GetUsers(ctx, logger, s.db, s.statusRegistry, []string{in.Id}, nil, nil)
	if err != nil {
		return nil, status.Error(codes.Internal, "An error occurred while trying to read user.")
	}
	if users == nil || len(users.Users) < 1 {
		// The user does not exist, so cannot have any friends to delete anyway.
		return &emptypb.Empty{}, nil
	}

	if err = DeleteFriends(ctx, logger, s.db, s.tracker, s.router, userID, users.Users[0].Username, []string{in.FriendId}); err != nil {
		// Error already logged in the function above.
		return nil, status.Error(codes.Internal, "An error occurred while trying to delete the friend relationship.")
	}

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) DeleteGroupUser(ctx context.Context, in *console.DeleteGroupUserRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	groupID, err := uuid.FromString(in.GroupId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid group ID.")
	}

	if err = KickGroupUsers(ctx, logger, s.db, s.tracker, s.router, s.streamManager, uuid.Nil, groupID, []uuid.UUID{userID}, true); err != nil {
		// Error already logged in the function above.
		if errors.Is(err, ErrEmptyMemberKick) {
			return nil, status.Error(codes.FailedPrecondition, "Cannot kick user from group.")
		}
		return nil, status.Error(codes.Internal, "An error occurred while trying to remove the user from the group.")
	}

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) DeleteWalletLedger(ctx context.Context, in *console.DeleteWalletLedgerRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	walletID, err := uuid.FromString(in.WalletId)
	if err != nil || walletID == uuid.Nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid wallet ledger item ID.")
	}

	query := "DELETE FROM wallet_ledger WHERE id = ? AND user_id = ?"
	_, err = s.db.ExecContext(ctx, query, walletID, userID)
	if err != nil {
		logger.Error("Error deleting from wallet ledger.", zap.String("id", walletID.String()), zap.String("user_id", userID.String()), zap.Error(err))
		return nil, status.Error(codes.Internal, "An error occurred while trying to remove the user's wallet ledger item.")
	}

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) ExportAccount(ctx context.Context, in *console.AccountId) (*console.AccountExport, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	export, err := ExportAccount(ctx, logger, s.db, userID)
	if err != nil {
		return nil, err
	}
	return export, nil
}

func (s *ConsoleServer) ImportAccount(ctx context.Context, in *console.AccountImport) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	{
		_, err := importAccountMySQL(ctx, logger, s.db, s.statusRegistry, userID, in.Data, s.storageIndex)
		if err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) ImportAccountFull(ctx context.Context, in *console.AccountImport) (*console.Account, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)

	return importAccountMySQL(ctx, logger, s.db, s.statusRegistry, uuid.Nil, in.Data, s.storageIndex)

}

func (s *ConsoleServer) GetAccount(ctx context.Context, in *console.AccountId) (*console.Account, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	account, err := GetAccount(ctx, logger, s.db, s.statusRegistry, userID)
	if err != nil {
		// Error already logged in function above.
		if errors.Is(err, ErrAccountNotFound) {
			return nil, status.Error(codes.NotFound, "Account not found.")
		}
		return nil, status.Error(codes.Internal, "An error occurred while trying to retrieve user account.")
	}

	account.User.DisplayName = html.EscapeString(account.User.DisplayName)
	account.User.AvatarUrl = html.EscapeString(account.User.AvatarUrl)
	account.User.LangTag = html.EscapeString(account.User.LangTag)
	account.User.Location = html.EscapeString(account.User.Location)
	account.User.Timezone = html.EscapeString(account.User.Timezone)

	return &console.Account{
		Account:     account,
		DisableTime: account.DisableTime,
	}, nil
}

func (s *ConsoleServer) GetFriends(ctx context.Context, in *console.AccountId) (*api.FriendList, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	friends, err := ListFriends(ctx, logger, s.db, s.statusRegistry, userID, 0, nil, "")
	if err != nil {
		// Error already logged in function above.
		return nil, status.Error(codes.Internal, "An error occurred while trying to list the user's friends.")
	}

	return friends, nil
}

func (s *ConsoleServer) GetGroups(ctx context.Context, in *console.AccountId) (*api.UserGroupList, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	groups, err := ListUserGroups(ctx, logger, s.db, userID, 0, nil, "")
	if err != nil {
		// Error already logged in function above.
		return nil, status.Error(codes.Internal, "An error occurred while trying to list the user's groups.")
	}

	return groups, nil
}

func (s *ConsoleServer) GetWalletLedger(ctx context.Context, in *console.GetWalletLedgerRequest) (*console.WalletLedgerList, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.AccountId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	limit := int(in.Limit)
	if limit < 1 || limit > 100 {
		return nil, status.Error(codes.InvalidArgument, "expects a limit value between 1 and 100")
	}

	var after time.Time
	if in.After != nil {
		after = in.After.AsTime()
	}
	var before time.Time
	if in.Before != nil {
		before = in.Before.AsTime()
	}

	ledger, nextCursorStr, prevCursorStr, err := ListWalletLedger(ctx, logger, s.db, userID, &limit, in.Cursor, after, before)
	if err != nil {
		// Error already logged in function above.
		return nil, status.Error(codes.Internal, "An error occurred while trying to list the user's wallet ledger.")
	}

	// Convert to console wire format.
	consoleLedger := make([]*console.WalletLedger, 0, len(ledger))
	for _, ledgerItem := range ledger {
		changeset, err := json.Marshal(ledgerItem.Changeset)
		if err != nil {
			logger.Error("Error encoding wallet ledger changeset.", zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to list the user's wallet ledger.")
		}
		metadata, err := json.Marshal(ledgerItem.Metadata)
		if err != nil {
			logger.Error("Error encoding wallet ledger metadata.", zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to list the user's wallet ledger.")
		}
		consoleLedger = append(consoleLedger, &console.WalletLedger{
			Id:         ledgerItem.ID,
			UserId:     ledgerItem.UserID,
			Changeset:  string(changeset),
			Metadata:   string(metadata),
			CreateTime: &timestamppb.Timestamp{Seconds: ledgerItem.CreateTime},
			UpdateTime: &timestamppb.Timestamp{Seconds: ledgerItem.UpdateTime},
		})
	}

	return &console.WalletLedgerList{Items: consoleLedger, NextCursor: nextCursorStr, PrevCursor: prevCursorStr}, nil
}

func (s *ConsoleServer) ListAccounts(ctx context.Context, in *console.ListAccountsRequest) (*console.AccountList, error) {

	return s.listAccountsMySQL(ctx, in)

}

func (s *ConsoleServer) UpdateAccount(ctx context.Context, in *console.UpdateAccountRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	if userID == uuid.Nil {
		return nil, status.Error(codes.InvalidArgument, "Cannot update the system user.")
	}

	statements := make([]string, 0)
	params := []interface{}{userID}

	if v := in.Username; v != nil {
		if len(v.Value) == 0 {
			return nil, status.Error(codes.InvalidArgument, "Username cannot be empty.")
		}
		if invalidUsernameRegex.MatchString(v.Value) {
			return nil, status.Error(codes.InvalidArgument, "Username cannot contain spaces or control characters.")
		}
		params = append(params, v.Value)
		statements = append(statements, "username = $"+strconv.Itoa(len(params)))
	}

	if v := in.DisplayName; v != nil {
		if d := v.Value; d == "" {
			statements = append(statements, "display_name = NULL")
		} else {
			params = append(params, d)
			statements = append(statements, "display_name = $"+strconv.Itoa(len(params)))
		}
	}

	if v := in.Metadata; v != nil && v.Value != "" {
		if maybeJSON := []byte(v.Value); !json.Valid(maybeJSON) || bytes.TrimSpace(maybeJSON)[0] != byteBracket {
			return nil, status.Error(codes.InvalidArgument, "Metadata must be a valid JSON object.")
		}
		params = append(params, v.Value)
		statements = append(statements, "metadata = $"+strconv.Itoa(len(params)))
	}

	if v := in.AvatarUrl; v != nil {
		if a := v.Value; a == "" {
			statements = append(statements, "avatar_url = NULL")
		} else {
			params = append(params, a)
			statements = append(statements, "avatar_url = $"+strconv.Itoa(len(params)))
		}
	}

	if v := in.LangTag; v != nil {
		if l := v.Value; l == "" {
			statements = append(statements, "lang_tag = NULL")
		} else {
			params = append(params, l)
			statements = append(statements, "lang_tag = $"+strconv.Itoa(len(params)))
		}
	}

	if v := in.Location; v != nil {
		if l := v.Value; l == "" {
			statements = append(statements, "location = NULL")
		} else {
			params = append(params, l)
			statements = append(statements, "location = $"+strconv.Itoa(len(params)))
		}
	}

	if v := in.Timezone; v != nil {
		if t := v.Value; t == "" {
			statements = append(statements, "timezone = NULL")
		} else {
			params = append(params, t)
			statements = append(statements, "timezone = $"+strconv.Itoa(len(params)))
		}
	}

	var removeCustomID bool
	if v := in.CustomId; v != nil {
		if c := v.Value; c == "" {
			removeCustomID = true
		} else {
			if invalidCharsRegex.MatchString(c) {
				return nil, status.Error(codes.InvalidArgument, "Custom ID invalid, no spaces or control characters allowed.")
			} else if len(c) < 6 || len(c) > 128 {
				return nil, status.Error(codes.InvalidArgument, "Custom ID invalid, must be 6-128 bytes.")
			} else {
				params = append(params, c)
				statements = append(statements, "custom_id = $"+strconv.Itoa(len(params)))
			}
		}
	}

	var removeEmail bool
	if v := in.Email; v != nil {
		if e := v.Value; e == "" {
			removeEmail = true
		} else {
			if invalidCharsRegex.MatchString(e) {
				return nil, status.Error(codes.InvalidArgument, "Invalid email address, no spaces or control characters allowed.")
			} else if !emailRegex.MatchString(e) {
				return nil, status.Error(codes.InvalidArgument, "Invalid email address format.")
			} else if len(e) < 10 || len(e) > 255 {
				return nil, status.Error(codes.InvalidArgument, "Invalid email address, must be 10-255 bytes.")
			} else {
				params = append(params, strings.ToLower(e))
				statements = append(statements, "email = $"+strconv.Itoa(len(params)))
			}
		}
	}

	var newPassword []byte
	if v := in.Password; v != nil {
		p := v.Value
		if len(p) < 8 {
			return nil, status.Error(codes.InvalidArgument, "Password must be at least 8 characters long.")
		}
		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(p), bcryptHashCost)
		if err != nil {
			logger.Error("Error hashing password.", zap.Error(err))
			return nil, status.Error(codes.Internal, "Error updating user account password.")
		}
		newPassword = hashedPassword
	}

	if v := in.Wallet; v != nil && v.Value != "" {
		var walletMap map[string]int64
		if err := json.Unmarshal([]byte(v.Value), &walletMap); err != nil || walletMap == nil {
			return nil, status.Error(codes.InvalidArgument, "Wallet must be a valid JSON object with only string keys and integer values.")
		}
		for k, v := range walletMap {
			if v < 0 {
				return nil, status.Errorf(codes.InvalidArgument, "Wallet rejected negative value at path '%v'.", k)
			}
		}
		params = append(params, v.Value)
		statements = append(statements, "wallet = $"+strconv.Itoa(len(params)))
	}

	for oldDeviceID, newDeviceID := range in.DeviceIds {
		if invalidCharsRegex.MatchString(oldDeviceID) {
			return nil, status.Error(codes.InvalidArgument, "Old device ID invalid, no spaces or control characters allowed.")
		} else if len(oldDeviceID) < 10 || len(oldDeviceID) > 128 {
			return nil, status.Error(codes.InvalidArgument, "Old device ID invalid, must be 10-128 bytes.")
		}

		if newDeviceID != "" {
			// Only validate if device ID is not being removed.
			if invalidCharsRegex.MatchString(newDeviceID) {
				return nil, status.Error(codes.InvalidArgument, "New device ID invalid, no spaces or control characters allowed.")
			} else if len(newDeviceID) < 10 || len(newDeviceID) > 128 {
				return nil, status.Error(codes.InvalidArgument, "New device ID invalid, must be 10-128 bytes.")
			}
		}
	}

	if len(statements) == 0 && !removeCustomID && !removeEmail && len(in.DeviceIds) == 0 && len(newPassword) == 0 {
		// Nothing to update.
		return &emptypb.Empty{}, nil
	}

	return s.updateAccountMySQL(ctx, userID, in, newPassword)

}

func (s *ConsoleServer) AddAccountNote(ctx context.Context, in *console.AddAccountNoteRequest) (*console.AccountNote, error) {
	consoleUserID, ok := ctx.Value(ctxConsoleUserIdKey{}).(uuid.UUID)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "Console user identifier not found in request context.")
	}
	consoleUsername, ok := ctx.Value(ctxConsoleUsernameKey{}).(string)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "Console username not found in request context.")
	}

	userID, err := uuid.FromString(in.AccountId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	if in.Note == "" {
		return nil, status.Error(codes.InvalidArgument, "Note cannot be empty.")
	}

	if in.Id == "" {
		// Create new note.
		in.Id = uuid.Must(uuid.NewV4()).String()
	}

	noteID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid note ID.")
	}
	in.Id = noteID.String()

	return s.writeAccountNoteMySQL(ctx, userID, consoleUserID, consoleUsername, in)

}

func (s *ConsoleServer) ListAccountNotes(ctx context.Context, in *console.ListAccountNotesRequest) (*console.ListAccountNotesResponse, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.AccountId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	if in.Limit < 1 || in.Limit > 100 {
		in.Limit = 10
	}

	var cursor *consoleAccountNotesCursor
	if in.Cursor != "" {
		cb, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil {
			logger.Error("Error decoding user account notes list cursor.", zap.String("cursor", in.Cursor), zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to decode user notes list request cursor.")
		}
		cursor = &consoleAccountNotesCursor{}
		if err := gob.NewDecoder(bytes.NewReader(cb)).Decode(&cursor); err != nil {
			logger.Error("Error decoding user account notes list cursor.", zap.String("cursor", in.Cursor), zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to decode user notes list request cursor.")
		}

		if cursor.UserID != userID {
			logger.Error("User identifier mismatch in user account notes list cursor.", zap.String("cursor", in.Cursor))
			return nil, status.Error(codes.InvalidArgument, "Cursor user identifier mismatch.")
		}
	}

	query := ""

	params := []interface{}{userID, in.Limit + 1}
	if cursor != nil {

		params = append(params, cursor.CreateTime, cursor.NoteID)
	}

	{
		query = "SELECT un.id,un.note,un.create_time,un.update_time,un.create_id,cuc.username,un.update_id,cuu.username FROM users_notes un LEFT JOIN console_user cuc ON un.create_id = cuc.id LEFT JOIN console_user cuu ON un.update_id = cuu.id WHERE user_id = ?"
		params = []any{userID}
		if cursor != nil {
			query += " AND (un.create_time,un.id) <= (?,?)"
			params = append(params, cursor.CreateTime, cursor.NoteID)
		}
		query += " ORDER BY un.create_time DESC, un.id DESC LIMIT ?"
		params = append(params, in.Limit+1)
	}

	rows, err := s.db.QueryContext(ctx, query, params...)
	if err != nil {
		logger.Error("Error querying user account notes.", zap.Error(err))
		return nil, status.Error(codes.Internal, "An error occurred while trying to list user notes.")
	}
	defer rows.Close()

	var newCursor *consoleAccountNotesCursor
	notes := make([]*console.AccountNote, 0, in.Limit)
	for rows.Next() {
		note := &console.AccountNote{}
		var createTime, updateTime sql.NullTime
		var createId, updateId, createUsername, updateUsername sql.NullString
		if err := rows.Scan(&note.Id, &note.Note, &createTime, &updateTime, &createId, &createUsername, &updateId, &updateUsername); err != nil {
			_ = rows.Close()
			logger.Error("Error scanning user account notes.", zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to list user notes.")
		}

		if len(notes) >= int(in.Limit) {
			newCursor = &consoleAccountNotesCursor{
				NoteID:     uuid.FromStringOrNil(note.Id),
				UserID:     userID,
				CreateTime: createTime.Time,
			}
			break
		}

		note.UserId = in.AccountId
		note.CreateTime = timestamppb.New(createTime.Time)
		note.UpdateTime = timestamppb.New(updateTime.Time)
		if createId.Valid {
			note.CreateId = createId.String
		}
		if createUsername.Valid {
			note.CreateUsername = createUsername.String
		}
		if updateId.Valid {
			note.UpdateId = updateId.String
		}
		if updateUsername.Valid {
			note.UpdateUsername = updateUsername.String
		}
		notes = append(notes, note)
	}
	_ = rows.Close()

	response := &console.ListAccountNotesResponse{Notes: notes}

	if newCursor != nil {
		cursorBuf := &bytes.Buffer{}
		if err := gob.NewEncoder(cursorBuf).Encode(newCursor); err != nil {
			logger.Error("Error encoding account notes cursor.", zap.Any("in", in), zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to list account notes.")
		}
		response.Cursor = base64.RawURLEncoding.EncodeToString(cursorBuf.Bytes())
	}

	return response, nil
}

func (s *ConsoleServer) DeleteAccountNote(ctx context.Context, in *console.DeleteAccountNoteRequest) (*emptypb.Empty, error) {
	_, err := uuid.FromString(in.NoteId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid note ID.")
	}
	_, err = uuid.FromString(in.AccountId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid account ID.")
	}

	{
		id, e := uuid.FromString(in.NoteId)
		if e != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid note ID.")
		}
		owner, e := uuid.FromString(in.AccountId)
		if e != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid account ID.")
		}
		if _, e = s.db.ExecContext(ctx, "DELETE FROM users_notes WHERE id = ? AND user_id = ?", id, owner); e != nil {
			return nil, status.Error(codes.Internal, "Could not delete note.")
		}
		return &emptypb.Empty{}, nil
	}

}
