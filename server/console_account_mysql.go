// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
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
	"slices"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/console"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const consoleAccountColumns = "id,username,display_name,avatar_url,lang_tag,location,timezone,metadata,apple_id,facebook_id,facebook_instant_game_id,google_id,gamecenter_id,steam_id,edge_count,create_time,update_time"

func (s *ConsoleServer) listAccountsMySQL(ctx context.Context, in *console.ListAccountsRequest) (*console.AccountList, error) {
	if in.Tombstones {
		q := "SELECT user_id,create_time FROM user_tombstone"
		args := []any{}
		if in.Filter != "" {
			id, err := uuid.FromString(in.Filter)
			if err != nil {
				return &console.AccountList{}, nil
			}
			q += " WHERE user_id = ?"
			args = append(args, id)
		}
		q += " ORDER BY user_id LIMIT 50"
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := &console.AccountList{}
		for rows.Next() {
			u := &api.User{}
			var t time.Time
			if err = rows.Scan(&u.Id, &t); err != nil {
				return nil, err
			}
			u.UpdateTime = timestamppb.New(t)
			out.Users = append(out.Users, u)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
		if in.Filter != "" {
			out.TotalCount = int32(len(out.Users))
		} else {
			out.TotalCount = countDatabase(ctx, s.logger, s.db, "user_tombstone")
		}
		return out, nil
	}
	q := "SELECT " + consoleAccountColumns + " FROM users"
	args := []any{}
	var c consoleAccountCursor
	filtered := in.Filter != ""
	if filtered {
		where := []string{}
		for _, column := range []string{"username", "facebook_id", "google_id", "gamecenter_id", "steam_id", "custom_id", "facebook_instant_game_id", "apple_id", "email"} {
			where = append(where, column+" = ?")
			args = append(args, in.Filter)
		}
		where = append(where, "id IN (SELECT user_id FROM user_device WHERE id = ?)")
		args = append(args, in.Filter)
		if id, err := uuid.FromString(in.Filter); err == nil {
			where = append(where, "id = ?")
			args = append(args, id)
		}
		// LOCATE treats %, _ and backslashes as literal display-name text.
		if len(in.Filter) >= 3 && !s.config.GetConsole().DisableDisplayNameSearch {
			where = append(where, "LOCATE(LOWER(?),LOWER(display_name)) > 0")
			args = append(args, in.Filter)
		}
		if strings.Contains(in.Filter, "%") && validTrigramFilterRegex.MatchString(in.Filter) {
			where = append(where, "LOWER(username) LIKE LOWER(?)")
			args = append(args, in.Filter)
		}
		q += " WHERE " + strings.Join(where, " OR ") + " ORDER BY id LIMIT 100"
	} else {
		if in.Cursor != "" {
			b, err := base64.RawURLEncoding.DecodeString(in.Cursor)
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, "Invalid cursor.")
			}
			if err = gob.NewDecoder(bytes.NewReader(b)).Decode(&c); err != nil {
				return nil, status.Error(codes.InvalidArgument, "Invalid cursor.")
			}
			q += " WHERE id > ?"
			args = append(args, c.ID)
		}
		q += " ORDER BY id LIMIT 51"
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &console.AccountList{}
	for rows.Next() {
		if !filtered && len(out.Users) == 50 {
			last := out.Users[49]
			b := &bytes.Buffer{}
			if err = gob.NewEncoder(b).Encode(&consoleAccountCursor{ID: uuid.FromStringOrNil(last.Id), Username: last.Username}); err != nil {
				return nil, err
			}
			out.NextCursor = base64.RawURLEncoding.EncodeToString(b.Bytes())
			break
		}
		u, err := convertUser(rows)
		if err != nil {
			return nil, err
		}
		u.DisplayName = html.EscapeString(u.DisplayName)
		u.AvatarUrl = html.EscapeString(u.AvatarUrl)
		u.LangTag = html.EscapeString(u.LangTag)
		u.Location = html.EscapeString(u.Location)
		u.Timezone = html.EscapeString(u.Timezone)
		out.Users = append(out.Users, u)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if s.statusRegistry != nil {
		s.statusRegistry.FillOnlineUsers(out.Users)
	}
	out.TotalCount = countDatabase(ctx, s.logger, s.db, "users")
	return out, nil
}
func (s *ConsoleServer) updateAccountMySQL(ctx context.Context, id uuid.UUID, in *console.UpdateAccountRequest, password []byte) (*emptypb.Empty, error) {
	err := executeInTxMySQL(ctx, s.db, func(tx *sql.Tx) error {
		var current string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", id).Scan(&current); err != nil {
			return err
		}
		fields := []string{}
		args := []any{}
		add := func(column string, v *wrapperspb.StringValue, nullable bool) {
			if v != nil {
				fields = append(fields, column+" = ?")
				if nullable && v.Value == "" {
					args = append(args, nil)
				} else {
					args = append(args, v.Value)
				}
			}
		}
		add("username", in.Username, false)
		add("display_name", in.DisplayName, true)
		add("avatar_url", in.AvatarUrl, true)
		add("lang_tag", in.LangTag, true)
		add("location", in.Location, true)
		add("timezone", in.Timezone, true)
		add("custom_id", in.CustomId, true)
		if in.Email != nil {
			add("email", wrapperspb.String(strings.ToLower(in.Email.Value)), true)
			if in.Email.Value == "" {
				fields = append(fields, "password = NULL")
			}
		}
		if in.Metadata != nil && in.Metadata.Value != "" {
			add("metadata", in.Metadata, false)
		}
		if in.Wallet != nil && in.Wallet.Value != "" {
			var wallet map[string]int64
			if err := json.Unmarshal([]byte(in.Wallet.Value), &wallet); err != nil || wallet == nil {
				return status.Error(codes.InvalidArgument, "Wallet must be a JSON object.")
			}
			add("wallet", in.Wallet, false)
		}
		if len(fields) > 0 {
			args = append(args, id)
			if _, err := tx.ExecContext(ctx, "UPDATE users SET "+strings.Join(fields, ",")+", update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", args...); err != nil {
				return err
			}
		}
		keys := make([]string, 0, len(in.DeviceIds))
		for k := range in.DeviceIds {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, old := range keys {
			newID := in.DeviceIds[old]
			var result sql.Result
			var err error
			if newID == "" {
				result, err = tx.ExecContext(ctx, "DELETE FROM user_device WHERE user_id = ? AND id = ?", id, old)
			} else {
				result, err = tx.ExecContext(ctx, "UPDATE user_device SET id = ? WHERE user_id = ? AND id = ?", newID, id, old)
			}
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return status.Error(codes.InvalidArgument, "Device ID is not linked to this account.")
			}
		}
		if len(password) > 0 {
			result, err := tx.ExecContext(ctx, "UPDATE users SET password = ? WHERE id = ? AND email IS NOT NULL", password, id)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return status.Error(codes.InvalidArgument, "Cannot set a password without an email address.")
			}
		}
		if len(keys) > 0 || in.CustomId != nil && in.CustomId.Value == "" || in.Email != nil && in.Email.Value == "" {
			// Check the final transaction state, including all simultaneous removals.
			var usable bool
			if err := tx.QueryRowContext(ctx, "SELECT (custom_id IS NOT NULL OR apple_id IS NOT NULL OR facebook_id IS NOT NULL OR facebook_instant_game_id IS NOT NULL OR google_id IS NOT NULL OR gamecenter_id IS NOT NULL OR steam_id IS NOT NULL OR (email IS NOT NULL AND password IS NOT NULL) OR EXISTS (SELECT 1 FROM user_device WHERE user_id = ?)) FROM users WHERE id = ?", id, id).Scan(&usable); err != nil {
				return err
			}
			if !usable {
				return status.Error(codes.InvalidArgument, "Cannot remove the last usable login identifier.")
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE users SET update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", id)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "Account not found.")
	}
	if mysqlErrorNumber(err, 1062) {
		return nil, status.Error(codes.AlreadyExists, "An account identifier is already in use.")
	}
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
func (s *ConsoleServer) writeAccountNoteMySQL(ctx context.Context, owner, author uuid.UUID, username string, in *console.AddAccountNoteRequest) (*console.AccountNote, error) {
	id, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid note ID.")
	}
	out := &console.AccountNote{Id: id.String(), UserId: owner.String(), Note: in.Note, UpdateUsername: username}
	err = executeInTxMySQL(ctx, s.db, func(tx *sql.Tx) error {
		var locked string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", owner).Scan(&locked); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO users_notes (user_id,id,note,create_id,update_id) VALUES (?,?,?,?,?)", owner, id, in.Note, author, author)
		if mysqlErrorNumber(err, 1062) {
			var existing uuid.UUID
			if err = tx.QueryRowContext(ctx, "SELECT user_id FROM users_notes WHERE id = ? FOR UPDATE", id).Scan(&existing); err != nil {
				return err
			}
			if existing != owner {
				return status.Error(codes.InvalidArgument, "Note belongs to a different account.")
			}
			_, err = tx.ExecContext(ctx, "UPDATE users_notes SET note = ?, update_id = ?, update_time = CURRENT_TIMESTAMP(6) WHERE id = ? AND user_id = ?", in.Note, author, id, owner)
		}
		if err != nil {
			return err
		}
		var created, updated time.Time
		var createdBy, createdName sql.NullString
		if err = tx.QueryRowContext(ctx, "SELECT un.create_time,un.update_time,un.create_id,cu.username FROM users_notes un LEFT JOIN console_user cu ON un.create_id = cu.id WHERE un.id = ?", id).Scan(&created, &updated, &createdBy, &createdName); err != nil {
			return err
		}
		out.CreateTime = timestamppb.New(created)
		out.UpdateTime = timestamppb.New(updated)
		out.CreateId = createdBy.String
		out.CreateUsername = createdName.String
		if author != uuid.Nil {
			out.UpdateId = author.String()
		}
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "Account not found.")
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}
