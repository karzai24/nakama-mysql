// Modified by nakama-mysql contributors: native MySQL friendship paths.
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
	"fmt"
	"strconv"

	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type edgeListCursor struct {
	// ID fields.
	State    int64
	Position int64
}

// Only used to get all friend IDs for the console. NOTE: Not intended for use in client/runtime APIs.
func GetFriendIDs(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID) (*api.FriendList, error) {
	query := "SELECT id, state FROM users JOIN user_edge ON id = destination_id WHERE source_id = ?"
	rows, err := db.QueryContext(ctx, query, userID)
	if err != nil {
		logger.Error("Error retrieving friends.", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	friends := make([]*api.Friend, 0, 10)

	for rows.Next() {
		var id string
		var state sql.NullInt64

		if err = rows.Scan(&id, &state); err != nil {
			logger.Error("Error retrieving friend IDs.", zap.Error(err))
			return nil, err
		}

		friendID := uuid.FromStringOrNil(id)
		user := &api.User{
			Id: friendID.String(),
		}

		friends = append(friends, &api.Friend{
			User: user,
			State: &wrapperspb.Int32Value{
				Value: int32(state.Int64),
			},
		})
	}
	if err = rows.Err(); err != nil {
		logger.Error("Error retrieving friend IDs.", zap.Error(err))
		return nil, err
	}

	return &api.FriendList{Friends: friends}, nil
}

func GetFriends(ctx context.Context, logger *zap.Logger, db *sql.DB, statusRegistry StatusRegistry, userID uuid.UUID, userIDs []uuid.UUID) ([]*api.Friend, error) {
	if len(userIDs) == 0 {
		return []*api.Friend{}, nil
	}

	uids := make([]any, len(userIDs))
	for i, uid := range userIDs {
		uids[i] = uid
	}
	query := mysqlFriendColumns + " AND destination_id IN (" + mysqlMarks(len(userIDs)) + ")"
	params := append([]any{userID}, uids...)
	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		logger.Error("Error retrieving friends.", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	friends := make([]*api.Friend, 0, len(userIDs))
	for rows.Next() {
		var id string
		var username sql.NullString
		var displayName sql.NullString
		var avatarURL sql.NullString
		var lang sql.NullString
		var location sql.NullString
		var timezone sql.NullString
		var userMetadata []byte
		var createTime sql.NullTime
		var updateTime sql.NullTime
		var edgeUpdateTime sql.NullTime
		var state sql.NullInt64
		var position sql.NullInt64
		var facebookID sql.NullString
		var googleID sql.NullString
		var gamecenterID sql.NullString
		var steamID sql.NullString
		var facebookInstantGameID sql.NullString
		var appleID sql.NullString
		var friendMetadata []byte

		if err = rows.Scan(&id, &username, &displayName, &avatarURL, &lang, &location, &timezone, &userMetadata,
			&createTime, &updateTime, &edgeUpdateTime, &state, &position,
			&facebookID, &googleID, &gamecenterID, &steamID, &facebookInstantGameID, &appleID, &friendMetadata); err != nil {
			logger.Error("Error retrieving friends.", zap.Error(err))
			return nil, err
		}

		user := &api.User{
			Id:          id,
			Username:    username.String,
			DisplayName: displayName.String,
			AvatarUrl:   avatarURL.String,
			LangTag:     lang.String,
			Location:    location.String,
			Timezone:    timezone.String,
			Metadata:    string(userMetadata),
			CreateTime:  &timestamppb.Timestamp{Seconds: createTime.Time.Unix()},
			UpdateTime:  &timestamppb.Timestamp{Seconds: updateTime.Time.Unix()},
			// Online filled below.
			FacebookId:            facebookID.String,
			GoogleId:              googleID.String,
			GamecenterId:          gamecenterID.String,
			SteamId:               steamID.String,
			FacebookInstantGameId: facebookInstantGameID.String,
			AppleId:               appleID.String,
		}

		friends = append(friends, &api.Friend{
			User: user,
			State: &wrapperspb.Int32Value{
				Value: int32(state.Int64),
			},
			UpdateTime: &timestamppb.Timestamp{Seconds: edgeUpdateTime.Time.Unix()},
			Metadata:   string(friendMetadata),
		})
	}
	if err = rows.Err(); err != nil {
		logger.Error("Error retrieving friends.", zap.Error(err))
		return nil, err
	}

	if statusRegistry != nil {
		statusRegistry.FillOnlineFriends(friends)
	}

	return friends, nil
}

func ListFriends(ctx context.Context, logger *zap.Logger, db *sql.DB, statusRegistry StatusRegistry, userID uuid.UUID, limit int, state *wrapperspb.Int32Value, cursor string) (*api.FriendList, error) {
	var incomingCursor *edgeListCursor
	if cursor != "" {
		cb, err := base64.URLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, runtime.ErrFriendInvalidCursor
		}
		incomingCursor = &edgeListCursor{}
		if err := gob.NewDecoder(bytes.NewReader(cb)).Decode(incomingCursor); err != nil {
			return nil, runtime.ErrFriendInvalidCursor
		}

		// Cursor and filter mismatch. Perhaps the caller has sent an old cursor with a changed filter.
		if state != nil && int64(state.Value) != incomingCursor.State {
			return nil, runtime.ErrFriendInvalidCursor
		}
	}

	params := make([]interface{}, 0, 4)
	query := ""

	params = append(params, userID)
	if state != nil {
		// Assumes the state has already been validated before this function.

		params = append(params, state.Value)
	}
	if incomingCursor != nil {

		if state == nil {
			params = append(params, incomingCursor.State)
		}
		params = append(params, incomingCursor.Position)
	}

	if limit != 0 {
		// Console API can select all friends in one request. Client/runtime calls will set a non-0 limit.
		params = append(params, limit+1)
		query += " LIMIT $" + strconv.Itoa(len(params))
	}

	{
		query, params = mysqlFriendColumns, []any{userID}
		if state != nil {
			query += " AND state = ?"
			params = append(params, state.Value)
		}
		if incomingCursor != nil {
			query += " AND (state, `position`) >= (?, ?)"
			params = append(params, incomingCursor.State, incomingCursor.Position)
		}
		query += " ORDER BY state, `position`"
		if limit != 0 {
			query += " LIMIT ?"
			params = append(params, limit+1)
		}
	}
	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		logger.Error("Error retrieving friends.", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	friends := make([]*api.Friend, 0, limit)
	var outgoingCursor string

	for rows.Next() {
		var id string
		var username sql.NullString
		var displayName sql.NullString
		var avatarURL sql.NullString
		var lang sql.NullString
		var location sql.NullString
		var timezone sql.NullString
		var userMetadata []byte
		var createTime sql.NullTime
		var updateTime sql.NullTime
		var edgeUpdateTime sql.NullTime
		var state sql.NullInt64
		var position sql.NullInt64
		var facebookID sql.NullString
		var googleID sql.NullString
		var gamecenterID sql.NullString
		var steamID sql.NullString
		var facebookInstantGameID sql.NullString
		var appleID sql.NullString
		var friendMetadata []byte

		if err = rows.Scan(&id, &username, &displayName, &avatarURL, &lang, &location, &timezone, &userMetadata,
			&createTime, &updateTime, &edgeUpdateTime, &state, &position,
			&facebookID, &googleID, &gamecenterID, &steamID, &facebookInstantGameID, &appleID, &friendMetadata); err != nil {
			logger.Error("Error retrieving friends.", zap.Error(err))
			return nil, err
		}

		if limit != 0 && len(friends) >= limit {
			cursorBuf := new(bytes.Buffer)
			if err := gob.NewEncoder(cursorBuf).Encode(&edgeListCursor{State: state.Int64, Position: position.Int64}); err != nil {
				logger.Error("Error creating friend list cursor", zap.Error(err))
				return nil, err
			}
			outgoingCursor = base64.URLEncoding.EncodeToString(cursorBuf.Bytes())
			break
		}

		user := &api.User{
			Id:          id,
			Username:    username.String,
			DisplayName: displayName.String,
			AvatarUrl:   avatarURL.String,
			LangTag:     lang.String,
			Location:    location.String,
			Timezone:    timezone.String,
			Metadata:    string(userMetadata),
			CreateTime:  &timestamppb.Timestamp{Seconds: createTime.Time.Unix()},
			UpdateTime:  &timestamppb.Timestamp{Seconds: updateTime.Time.Unix()},
			// Online filled below.
			FacebookId:            facebookID.String,
			GoogleId:              googleID.String,
			GamecenterId:          gamecenterID.String,
			SteamId:               steamID.String,
			FacebookInstantGameId: facebookInstantGameID.String,
			AppleId:               appleID.String,
		}

		friends = append(friends, &api.Friend{
			User: user,
			State: &wrapperspb.Int32Value{
				Value: int32(state.Int64),
			},
			UpdateTime: &timestamppb.Timestamp{Seconds: edgeUpdateTime.Time.Unix()},
			Metadata:   string(friendMetadata),
		})
	}
	if err = rows.Err(); err != nil {
		logger.Error("Error retrieving friends.", zap.Error(err))
		return nil, err
	}

	if statusRegistry != nil {
		statusRegistry.FillOnlineFriends(friends)
	}

	return &api.FriendList{Friends: friends, Cursor: outgoingCursor}, nil
}

type friendsOfFriendsListCursor struct {
	SourceId      string
	DestinationId string
}

func ListFriendsOfFriends(ctx context.Context, logger *zap.Logger, db *sql.DB, statusRegistry StatusRegistry, userID uuid.UUID, limit int, cursor string) (*api.FriendsOfFriendsList, error) {

	return listFriendsOfFriendsMySQL(ctx, logger, db, statusRegistry, userID, limit, cursor)

}

func AddFriends(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, messageRouter MessageRouter, userID uuid.UUID, username string, friendIDs []string, metadata string) error {
	var notificationToSend map[string]bool
	write := func() error {
		var err error
		{
			notificationToSend, err = mutateFriendsMySQL(ctx, db, userID, friendIDs, "add", metadata)
		}

		return err
	}

	if err := write(); err != nil {
		logger.Error("Error adding friends.", zap.Error(err))
		return err
	}

	notifications := make(map[uuid.UUID][]*api.Notification)
	content, _ := json.Marshal(map[string]interface{}{"username": username})
	for id, isFriendAccept := range notificationToSend {
		uid := uuid.FromStringOrNil(id)
		code := NotificationCodeFriendRequest
		subject := fmt.Sprintf("%v wants to add you as a friend", username)
		if isFriendAccept {
			code = NotificationCodeFriendAccept
			subject = fmt.Sprintf("%v accepted your friend request", username)
		}
		notifications[uid] = []*api.Notification{{
			Id:         uuid.Must(uuid.NewV4()).String(),
			Subject:    subject,
			Content:    string(content),
			SenderId:   userID.String(),
			Code:       code,
			Persistent: true,
			CreateTime: &timestamppb.Timestamp{Seconds: time.Now().UTC().Unix()},
		}}
	}

	// Any error is already logged before it's returned here.
	_ = NotificationSend(ctx, logger, db, tracker, messageRouter, notifications)

	return nil
}

func UpdateFriendMetadata(ctx context.Context, logger *zap.Logger, db *sql.DB, userID, friendUserID uuid.UUID, metadata map[string]any) error {
	metadataStr := "{}"
	if metadata != nil {
		metadataBytes, err := json.Marshal(metadata)
		if err != nil {
			logger.Error("Failed to marshal friend metadata", zap.Error(err))
			return err
		}
		metadataStr = string(metadataBytes)
	}

	{
		_, err := mutateFriendsMySQL(ctx, db, userID, []string{friendUserID.String()}, "metadata", metadataStr)
		return err
	}

}

func DeleteFriends(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, messageRouter MessageRouter, userID uuid.UUID, username string, friendIDs []string) error {
	var notificationToSend map[string]bool
	write := func() error {
		var err error
		{
			notificationToSend, err = mutateFriendsMySQL(ctx, db, userID, friendIDs, "delete", "")
		}

		return err
	}

	if err := write(); err != nil {
		logger.Error("Error deleting friends.", zap.Error(err))
		return err
	}

	notifications := make(map[uuid.UUID][]*api.Notification)
	content, _ := json.Marshal(map[string]interface{}{"username": username})
	for id := range notificationToSend {
		uid := uuid.FromStringOrNil(id)
		notifications[uid] = []*api.Notification{{
			Id:         uuid.Must(uuid.NewV4()).String(),
			Subject:    fmt.Sprintf("%v removed you as a friend", username),
			Content:    string(content),
			SenderId:   userID.String(),
			Code:       NotificationCodeFriendRemove,
			Persistent: true,
			CreateTime: &timestamppb.Timestamp{Seconds: time.Now().UTC().Unix()},
		}}
	}

	// Any error is already logged before it's returned here.
	_ = NotificationSend(ctx, logger, db, tracker, messageRouter, notifications)

	return nil
}

func BlockFriends(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, currentUser uuid.UUID, ids []string) error {
	mutate := mutateFriendsMySQL
	{
		mutate = mutateFriendsMySQL
	}
	changed, err := mutate(ctx, db, currentUser, ids, "block", "")
	if err != nil {
		return err
	}
	for id := range changed {
		peer := uuid.Must(uuid.FromString(id))
		stream := PresenceStream{Mode: StreamModeDM, Subject: currentUser, Subcontext: peer}
		if id < currentUser.String() {
			stream.Subject, stream.Subcontext = peer, currentUser
		}
		tracker.UntrackByStream(stream)
	}
	return nil
}
