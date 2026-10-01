// Modified by nakama-mysql contributors: native MySQL backend dispatch.
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
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/console"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

var ErrAccountNotFound = errors.New("account not found")

// Not an API entity, only used to receive data from runtime environment.
type accountUpdate struct {
	userID      uuid.UUID
	username    string
	displayName *wrapperspb.StringValue
	timezone    *wrapperspb.StringValue
	location    *wrapperspb.StringValue
	langTag     *wrapperspb.StringValue
	avatarURL   *wrapperspb.StringValue
	metadata    *wrapperspb.StringValue
}

func GetAccount(ctx context.Context, logger *zap.Logger, db *sql.DB, statusRegistry StatusRegistry, userID uuid.UUID) (*api.Account, error) {
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
	var deviceIDs []string

	query := ""

	var mysqlDevices string
	var deviceScanner any = &mysqlDevices
	{
		query = `SELECT u.username, u.display_name, u.avatar_url, u.lang_tag, u.location, u.timezone, u.metadata, u.wallet,
            u.email, u.apple_id, u.facebook_id, u.facebook_instant_game_id, u.google_id, u.gamecenter_id, u.steam_id, u.custom_id, u.edge_count,
            u.create_time, u.update_time, u.verify_time, u.disable_time,
            COALESCE((SELECT JSON_ARRAYAGG(ud.id) FROM user_device ud WHERE u.id = ud.user_id), JSON_ARRAY())
            FROM users u WHERE u.id = ?`
		deviceScanner = &mysqlDevices
	}
	if err := db.QueryRowContext(ctx, query, userID).Scan(&username, &displayName, &avatarURL, &langTag, &location, &timezone, &metadata, &wallet, &email, &apple, &facebook, &facebookInstantGame, &google, &gamecenter, &steam, &customID, &edgeCount, &createTime, &updateTime, &verifyTime, &disableTime, deviceScanner); err != nil {
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
	devices := make([]*api.AccountDevice, 0, len(deviceIDs))
	for _, deviceID := range deviceIDs {
		devices = append(devices, &api.AccountDevice{Id: deviceID})
	}

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
		CustomId:    customID.String,
		VerifyTime:  verifyTimestamp,
		DisableTime: disableTimestamp,
	}, nil
}

func GetAccounts(ctx context.Context, logger *zap.Logger, db *sql.DB, statusRegistry StatusRegistry, userIDs, deviceIDs []string) ([]*api.Account, error) {
	if len(userIDs) == 0 && len(deviceIDs) == 0 {
		return []*api.Account{}, nil
	}

	canonical, err := canonicalUserIDsMySQL(userIDs)
	if err != nil {
		return nil, err
	}
	query, params := accountLookupMySQL(canonical, deviceIDs)
	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		logger.Error("Error retrieving user accounts.", zap.Error(err))
		return nil, err
	}

	accounts := make([]*api.Account, 0, len(userIDs))
	for rows.Next() {
		var userID string
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
		var deviceIDs []string

		var mysqlDevices string
		var deviceScanner any = &mysqlDevices
		{
			deviceScanner = &mysqlDevices
		}
		err = rows.Scan(&userID, &username, &displayName, &avatarURL, &langTag, &location, &timezone, &metadata, &wallet, &email, &apple, &facebook, &facebookInstantGame, &google, &gamecenter, &steam, &customID, &edgeCount, &createTime, &updateTime, &verifyTime, &disableTime, deviceScanner)
		if err != nil {
			_ = rows.Close()
			logger.Error("Error retrieving user accounts.", zap.Error(err))
			return nil, err
		}

		{
			if err := json.Unmarshal([]byte(mysqlDevices), &deviceIDs); err != nil {
				_ = rows.Close()
				return nil, err
			}
		}

		devices := make([]*api.AccountDevice, 0, len(deviceIDs))
		for _, deviceID := range deviceIDs {
			devices = append(devices, &api.AccountDevice{Id: deviceID})
		}

		var verifyTimestamp *timestamppb.Timestamp
		if verifyTime.Valid && verifyTime.Time.Unix() != 0 {
			verifyTimestamp = &timestamppb.Timestamp{Seconds: verifyTime.Time.Unix()}
		}
		var disableTimestamp *timestamppb.Timestamp
		if disableTime.Valid && disableTime.Time.Unix() != 0 {
			disableTimestamp = &timestamppb.Timestamp{Seconds: disableTime.Time.Unix()}
		}

		accounts = append(accounts, &api.Account{
			User: &api.User{
				Id:                    userID,
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
				// Online filled below.
			},
			Wallet:      wallet.String,
			Email:       email.String,
			Devices:     devices,
			CustomId:    customID.String,
			VerifyTime:  verifyTimestamp,
			DisableTime: disableTimestamp,
		})
	}
	_ = rows.Close()

	if err := rows.Err(); err != nil {
		return nil, err
	}

	if statusRegistry != nil {
		statusRegistry.FillOnlineAccounts(accounts)
	}

	return accounts, nil
}

func UpdateAccounts(ctx context.Context, logger *zap.Logger, db *sql.DB, updates []*accountUpdate) error {

	return updateAccountsMySQL(ctx, db, updates)

}

func ExportAccount(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID) (*console.AccountExport, error) {
	// Core user account.
	account, err := GetAccount(ctx, logger, db, nil, userID)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			return nil, status.Error(codes.NotFound, "Account not found.")
		}
		logger.Error("Could not export account data", zap.Error(err), zap.String("user_id", userID.String()))
		return nil, status.Error(codes.Internal, "An error occurred while trying to export user data.")
	}

	var (
		friends            *api.FriendList
		messages           []*api.ChannelMessage
		leaderboardRecords []*api.LeaderboardRecord
		groups             []*api.Group
		notifications      *api.NotificationList
		walletLedgers      []*console.WalletLedger
	)

	if userID != uuid.Nil {
		// Friends.
		friends, err = GetFriendIDs(ctx, logger, db, userID)
		if err != nil {
			logger.Error("Could not fetch friend IDs", zap.Error(err), zap.String("user_id", userID.String()))
			return nil, status.Error(codes.Internal, "An error occurred while trying to export user data.")
		}

		// Messages.
		messages, err = GetChannelMessages(ctx, logger, db, userID)
		if err != nil {
			logger.Error("Could not fetch messages", zap.Error(err), zap.String("user_id", userID.String()))
			return nil, status.Error(codes.Internal, "An error occurred while trying to export user data.")
		}

		// Leaderboard records.
		leaderboardRecords, err = LeaderboardRecordReadAll(ctx, logger, db, userID)
		if err != nil {
			logger.Error("Could not fetch leaderboard records", zap.Error(err), zap.String("user_id", userID.String()))
			return nil, status.Error(codes.Internal, "An error occurred while trying to export user data.")
		}

		groupUsers, err := ListUserGroups(ctx, logger, db, userID, 0, nil, "")
		if err != nil {
			logger.Error("Could not fetch groups that belong to the user", zap.Error(err), zap.String("user_id", userID.String()))
			return nil, status.Error(codes.Internal, "An error occurred while trying to export user data.")
		}
		if len(groupUsers.UserGroups) > 0 {
			groups = make([]*api.Group, 0, len(groupUsers.UserGroups))
		}
		for _, g := range groupUsers.UserGroups {
			groups = append(groups, g.Group)
		}

		// Notifications.
		notifications, err = NotificationList(ctx, logger, db, userID, 0, "", true)
		if err != nil {
			logger.Error("Could not fetch notifications", zap.Error(err), zap.String("user_id", userID.String()))
			return nil, status.Error(codes.Internal, "An error occurred while trying to export user data.")
		}

		// History of user's wallet.
		wl, _, _, err := ListWalletLedger(ctx, logger, db, userID, nil, "", time.Time{}, time.Time{})
		if err != nil {
			logger.Error("Could not fetch wallet ledger items", zap.Error(err), zap.String("user_id", userID.String()))
			return nil, status.Error(codes.Internal, "An error occurred while trying to export user data.")
		}
		if len(wl) > 0 {
			walletLedgers = make([]*console.WalletLedger, len(wl))
		}
		for i, w := range wl {
			changeset, err := json.Marshal(w.Changeset)
			if err != nil {
				logger.Error("Could not fetch wallet ledger items, error encoding changeset", zap.Error(err), zap.String("user_id", userID.String()))
				return nil, status.Error(codes.Internal, "An error occurred while trying to export user data.")
			}
			metadata, err := json.Marshal(w.Metadata)
			if err != nil {
				logger.Error("Could not fetch wallet ledger items, error encoding metadata", zap.Error(err), zap.String("user_id", userID.String()))
				return nil, status.Error(codes.Internal, "An error occurred while trying to export user data.")
			}
			walletLedgers[i] = &console.WalletLedger{
				Id:         w.ID,
				UserId:     w.UserID,
				Changeset:  string(changeset),
				Metadata:   string(metadata),
				CreateTime: &timestamppb.Timestamp{Seconds: w.CreateTime},
				UpdateTime: &timestamppb.Timestamp{Seconds: w.UpdateTime},
			}
		}
	}

	// Storage objects where user is the owner.
	storageObjects, err := StorageReadAllUserObjects(ctx, logger, db, userID)
	if err != nil {
		logger.Error("Could not fetch notifications", zap.Error(err), zap.String("user_id", userID.String()))
		return nil, status.Error(codes.Internal, "An error occurred while trying to export user data.")
	}

	export := &console.AccountExport{
		Account:            account,
		Objects:            storageObjects,
		Friends:            friends.GetFriends(),
		Messages:           messages,
		Groups:             groups,
		LeaderboardRecords: leaderboardRecords,
		Notifications:      notifications.GetNotifications(),
		WalletLedgers:      walletLedgers,
	}

	return export, nil
}

func ImportAccount(ctx context.Context, logger *zap.Logger, db *sql.DB, statusRegistry StatusRegistry, userID uuid.UUID, data *console.AccountExport, indexes ...StorageIndex) (*console.Account, error) {
	{
		var index StorageIndex
		if len(indexes) > 0 {
			index = indexes[0]
		}
		return importAccountMySQL(ctx, logger, db, statusRegistry, userID, data, index)
	}

}

func DeleteAccount(ctx context.Context, logger *zap.Logger, db *sql.DB, config Config, leaderboardCache LeaderboardCache, leaderboardRankCache LeaderboardRankCache, sessionRegistry SessionRegistry, sessionCache SessionCache, tracker Tracker, userID uuid.UUID, recorded bool, storageIndex StorageIndex) error {
	if userID == uuid.Nil {
		return errors.New("cannot delete the system user")
	}

	return deleteAccountMySQL(ctx, logger, db, config, leaderboardRankCache, sessionRegistry, sessionCache, tracker, storageIndex, userID, recorded)

}
