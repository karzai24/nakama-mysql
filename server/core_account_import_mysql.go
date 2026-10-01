// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/console"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func getAccountTxMySQL(ctx context.Context, logger *zap.Logger, tx *sql.Tx, statusRegistry StatusRegistry, userID uuid.UUID) (*api.Account, error) {
	var username sql.NullString
	var displayName sql.NullString
	var avatarURL sql.NullString
	var langTag sql.NullString
	var location sql.NullString
	var timezone sql.NullString
	var metadata sql.NullString
	var wallet sql.NullString
	var email sql.NullString
	var apple sql.NullString
	var facebook sql.NullString
	var facebookInstantGame sql.NullString
	var google sql.NullString
	var gamecenter sql.NullString
	var steam sql.NullString
	var customID sql.NullString
	var edgeCount int
	var createTime sql.NullTime
	var updateTime sql.NullTime
	var verifyTime sql.NullTime
	var disableTime sql.NullTime
	var deviceIDs []mysqlAccountIdentity

	var mysqlDevices string
	query := `SELECT u.username,u.display_name,u.avatar_url,u.lang_tag,u.location,u.timezone,u.metadata,u.wallet,
    u.email,u.apple_id,u.facebook_id,u.facebook_instant_game_id,u.google_id,u.gamecenter_id,u.steam_id,u.custom_id,u.edge_count,
    u.create_time,u.update_time,u.verify_time,u.disable_time,
    COALESCE((SELECT JSON_ARRAYAGG(JSON_OBJECT('provider',ud.provider,'id',ud.id)) FROM user_device ud WHERE ud.user_id = u.id),JSON_ARRAY()) FROM users u WHERE u.id = ?`
	var deviceScanner any = &mysqlDevices
	if err := tx.QueryRowContext(ctx, query, userID).Scan(&username, &displayName, &avatarURL, &langTag, &location, &timezone, &metadata, &wallet, &email, &apple, &facebook, &facebookInstantGame, &google, &gamecenter, &steam, &customID, &edgeCount, &createTime, &updateTime, &verifyTime, &disableTime, deviceScanner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		logger.Error("Error retrieving user account.", zap.Error(err))
		return nil, err
	}

	{
		if err := json.Unmarshal([]byte(mysqlDevices), &deviceIDs); err != nil {
			return nil, err
		}
	}
	devices, providers := splitMySQLAccountIdentities(deviceIDs)

	var verifyTimestamp *timestamppb.Timestamp
	if verifyTime.Valid && verifyTime.Time.Unix() != 0 {
		verifyTimestamp = &timestamppb.Timestamp{Seconds: verifyTime.Time.Unix()}
	}
	var disableTimestamp *timestamppb.Timestamp
	if disableTime.Valid && disableTime.Time.Unix() != 0 {
		disableTimestamp = &timestamppb.Timestamp{Seconds: disableTime.Time.Unix()}
	}

	online := false
	if statusRegistry != nil {
		online = statusRegistry.IsOnline(userID)
	}

	return &api.Account{
		User: &api.User{
			Id:                    userID.String(),
			Username:              username.String,
			DisplayName:           displayName.String,
			AvatarUrl:             avatarURL.String,
			LangTag:               langTag.String,
			Location:              location.String,
			Timezone:              timezone.String,
			Metadata:              metadata.String,
			AppleId:               apple.String,
			FacebookId:            facebook.String,
			FacebookInstantGameId: facebookInstantGame.String,
			GoogleId:              google.String,
			GamecenterId:          gamecenter.String,
			SteamId:               steam.String,
			EdgeCount:             int32(edgeCount),
			CreateTime:            &timestamppb.Timestamp{Seconds: createTime.Time.Unix()},
			UpdateTime:            &timestamppb.Timestamp{Seconds: updateTime.Time.Unix()},
			Online:                online,
		},
		Wallet:      wallet.String,
		Email:       email.String,
		Devices:     devices,
		Providers:   providers,
		CustomId:    customID.String,
		VerifyTime:  verifyTimestamp,
		DisableTime: disableTimestamp,
	}, nil
}

// Import retains Nakama's contract: a new profile includes its identifiers, while
// an existing profile replaces metadata, wallet and storage. Other exported data
// (friends, chat, groups and ranks) is informational and is not imported upstream.
func importAccountMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, registry StatusRegistry, target uuid.UUID, data *console.AccountExport, index StorageIndex) (*console.Account, error) {
	if data == nil || data.Account == nil || data.Account.User == nil {
		return nil, status.Error(codes.InvalidArgument, "An account is required.")
	}
	source, err := uuid.FromString(data.Account.User.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid imported user ID.")
	}
	create := target == uuid.Nil && source != uuid.Nil
	owner := target
	if create {
		owner = source
	}
	objectJSON := func(value string) (string, error) {
		if strings.TrimSpace(value) == "" {
			return "{}", nil
		}
		var v map[string]json.RawMessage
		if err := json.Unmarshal([]byte(value), &v); err != nil || v == nil {
			return "", status.Error(codes.InvalidArgument, "Expected a JSON object.")
		}
		return value, nil
	}
	metadata, err := objectJSON(data.Account.User.Metadata)
	if err != nil {
		return nil, err
	}
	wallet, err := objectJSON(data.Account.Wallet)
	if err != nil {
		return nil, err
	}
	var balances map[string]int64
	if err = json.Unmarshal([]byte(wallet), &balances); err != nil {
		return nil, status.Error(codes.InvalidArgument, "Wallet values must be integers.")
	}
	for _, n := range balances {
		if n < 0 {
			return nil, status.Error(codes.InvalidArgument, "Wallet cannot be negative.")
		}
	}
	for _, o := range data.Objects {
		if o == nil || o.Collection == "" || o.Key == "" || o.Version == "" || o.PermissionRead < 0 || o.PermissionRead > 2 || o.PermissionWrite < 0 || o.PermissionWrite > 1 {
			return nil, status.Error(codes.InvalidArgument, "Invalid imported storage object.")
		}
		if _, err = objectJSON(o.Value); err != nil {
			return nil, err
		}
		if o.CreateTime == nil || o.UpdateTime == nil || o.CreateTime.CheckValid() != nil || o.UpdateTime.CheckValid() != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid storage timestamp.")
		}
	}
	var account *api.Account
	var removed StorageOpDeletes
	var written []*api.StorageObject
	err = executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		removed = nil
		written = nil
		nullable := func(v string) any {
			v = strings.TrimSpace(v)
			if v == "" {
				return nil
			}
			return v
		}
		stamp := func(t *timestamppb.Timestamp) time.Time {
			if t == nil {
				return time.Unix(0, 0).UTC()
			}
			return t.AsTime()
		}
		u := data.Account.User
		if create {
			if u.Username == "" || invalidUsernameRegex.MatchString(u.Username) {
				return status.Error(codes.InvalidArgument, "Invalid username.")
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO users (id,username,display_name,avatar_url,lang_tag,location,timezone,metadata,wallet,email,facebook_id,google_id,gamecenter_id,steam_id,custom_id,create_time,update_time,verify_time,disable_time,facebook_instant_game_id,apple_id) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, owner, u.Username, nullable(u.DisplayName), nullable(u.AvatarUrl), u.LangTag, nullable(u.Location), nullable(u.Timezone), metadata, wallet, nullable(data.Account.Email), nullable(u.FacebookId), nullable(u.GoogleId), nullable(u.GamecenterId), nullable(u.SteamId), nullable(data.Account.CustomId), stamp(u.CreateTime), stamp(u.UpdateTime), stamp(data.Account.VerifyTime), stamp(data.Account.DisableTime), nullable(u.FacebookInstantGameId), nullable(u.AppleId))
			if err != nil {
				return err
			}
			for _, d := range data.Account.Devices {
				if d == nil || d.Id == "" {
					return status.Error(codes.InvalidArgument, "Invalid device ID.")
				}
				if _, err = tx.ExecContext(ctx, "INSERT INTO user_device (id,user_id) VALUES (?,?)", d.Id, owner); err != nil {
					return err
				}
			}
			for _, identity := range data.Account.Providers {
				if identity == nil || identity.Provider == "" || identity.ProviderUserId == "" {
					return status.Error(codes.InvalidArgument, "Invalid provider identity.")
				}
				if _, err = tx.ExecContext(ctx, "INSERT INTO user_device (provider,id,user_id) VALUES (?,?,?)", strings.ToLower(identity.Provider), identity.ProviderUserId, owner); err != nil {
					return err
				}
			}
		} else {
			var locked string
			if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", owner).Scan(&locked); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE users SET metadata = ?,wallet = ?,update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", metadata, wallet, owner); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, "SELECT collection,`key` FROM storage WHERE user_id = ?", owner)
			if err != nil {
				return err
			}
			for rows.Next() {
				o := &StorageOpDelete{OwnerID: owner.String(), ObjectID: &api.DeleteStorageObjectId{}}
				if err = rows.Scan(&o.ObjectID.Collection, &o.ObjectID.Key); err != nil {
					rows.Close()
					return err
				}
				removed = append(removed, o)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM storage WHERE user_id = ?", owner); err != nil {
				return err
			}
		}
		for _, o := range data.Objects {
			_, err := tx.ExecContext(ctx, "INSERT INTO storage (user_id,collection,`key`,`value`,version,`read`,`write`,create_time,update_time) VALUES (?,?,?,?,?,?,?,?,?)", owner, o.Collection, o.Key, o.Value, o.Version, o.PermissionRead, o.PermissionWrite, o.CreateTime.AsTime(), o.UpdateTime.AsTime())
			if err != nil {
				return err
			}
			written = append(written, &api.StorageObject{UserId: owner.String(), Collection: o.Collection, Key: o.Key, Value: o.Value, Version: o.Version, PermissionRead: o.PermissionRead, PermissionWrite: o.PermissionWrite, CreateTime: o.CreateTime, UpdateTime: o.UpdateTime})
		}
		var err error
		account, err = getAccountTxMySQL(ctx, logger, tx, registry, owner)
		return err
	})
	if mysqlErrorNumber(err, 1062) {
		return nil, status.Error(codes.AlreadyExists, "An imported account, identifier or storage key already exists.")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "Account not found.")
	}
	if err != nil {
		return nil, err
	}
	if index != nil {
		index.Delete(ctx, removed)
		index.Write(ctx, written)
	}
	return &console.Account{Account: account, DisableTime: account.DisableTime}, nil
}
