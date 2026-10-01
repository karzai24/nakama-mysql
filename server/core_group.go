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

	"errors"

	"math"
	"strconv"

	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"

	"github.com/heroiclabs/nakama-common/runtime"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

var ErrEmptyMemberDemote = errors.New("could not demote member")
var ErrEmptyMemberPromote = errors.New("could not promote member")
var ErrEmptyMemberKick = errors.New("could not kick member")

const BANNED_CODE = 4

type groupListCursor struct {
	Lang       string
	EdgeCount  int32
	ID         uuid.UUID
	Open       bool
	Name       string
	UpdateTime int64
}

func (c *groupListCursor) GetState() int {
	if c.Open {
		return 0
	}
	return 1
}
func (c *groupListCursor) GetUpdateTime() time.Time {
	return time.Unix(0, c.UpdateTime)
}

func CreateGroup(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, creatorID uuid.UUID, name, lang, desc, avatarURL, metadata string, open bool, maxCount int) (*api.Group, error) {

	return createGroupMySQL(ctx, db, userID, creatorID, name, lang, desc, avatarURL, metadata, open, maxCount)

}

func UpdateGroup(ctx context.Context, logger *zap.Logger, db *sql.DB, groupID uuid.UUID, userID uuid.UUID, creatorID uuid.UUID, name, lang, desc, avatar, metadata *wrapperspb.StringValue, open *wrapperspb.BoolValue, maxCount int) error {

	return updateGroupMySQL(ctx, db, groupID, userID, creatorID, name, lang, desc, avatar, metadata, open, maxCount)

}

func DeleteGroup(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, groupID uuid.UUID, userID uuid.UUID) error {

	return deleteGroupWithPermissionMySQL(ctx, db, tracker, groupID, userID)

}

func JoinGroup(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, router MessageRouter, groupID uuid.UUID, userID uuid.UUID, username string) error {

	return mutateGroupMySQL(ctx, logger, db, tracker, router, nil, userID, groupID, []uuid.UUID{userID}, username, "join", false)

}

func LeaveGroup(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, router MessageRouter, streamManager StreamManager, groupID uuid.UUID, userID uuid.UUID, username string) error {

	return mutateGroupMySQL(ctx, logger, db, tracker, router, streamManager, userID, groupID, []uuid.UUID{userID}, username, "leave", false)

}

func AddGroupUsers(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, router MessageRouter, caller uuid.UUID, groupID uuid.UUID, userIDs []uuid.UUID) error {

	return mutateGroupMySQL(ctx, logger, db, tracker, router, nil, caller, groupID, userIDs, "", "add", false)

}

func BanGroupUsers(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, router MessageRouter, streamManager StreamManager, caller uuid.UUID, groupID uuid.UUID, userIDs []uuid.UUID) error {

	return mutateGroupMySQL(ctx, logger, db, tracker, router, streamManager, caller, groupID, userIDs, "", "ban", false)

}

func KickGroupUsers(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, router MessageRouter, streamManager StreamManager, caller uuid.UUID, groupID uuid.UUID, userIDs []uuid.UUID, strictError bool) error {

	return mutateGroupMySQL(ctx, logger, db, tracker, router, streamManager, caller, groupID, userIDs, "", "kick", strictError)

}

func PromoteGroupUsers(ctx context.Context, logger *zap.Logger, db *sql.DB, router MessageRouter, caller uuid.UUID, groupID uuid.UUID, userIDs []uuid.UUID) error {

	return mutateGroupMySQL(ctx, logger, db, nil, router, nil, caller, groupID, userIDs, "", "promote", false)

}

func DemoteGroupUsers(ctx context.Context, logger *zap.Logger, db *sql.DB, router MessageRouter, caller uuid.UUID, groupID uuid.UUID, userIDs []uuid.UUID) error {

	return mutateGroupMySQL(ctx, logger, db, nil, router, nil, caller, groupID, userIDs, "", "demote", false)

}

func ListGroupUsers(ctx context.Context, logger *zap.Logger, db *sql.DB, statusRegistry StatusRegistry, groupID uuid.UUID, limit int, state *wrapperspb.Int32Value, cursor string) (*api.GroupUserList, error) {
	var incomingCursor *edgeListCursor
	if cursor != "" {
		cb, err := base64.URLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, runtime.ErrGroupUserInvalidCursor
		}
		incomingCursor = &edgeListCursor{}
		if err := gob.NewDecoder(bytes.NewReader(cb)).Decode(incomingCursor); err != nil {
			return nil, runtime.ErrGroupUserInvalidCursor
		}

		// Cursor and filter mismatch. Perhaps the caller has sent an old cursor with a changed filter.
		if state != nil && int64(state.Value) != incomingCursor.State {
			return nil, runtime.ErrGroupUserInvalidCursor
		}
	}

	params := make([]interface{}, 0, 4)
	query := ""

	params = append(params, groupID)
	if state != nil {
		// Assumes the state has already been validated before this function.

		params = append(params, state.Value)
	} else {
		// Hint for the query analyzer to ensure it performs and index scan on the appropriate range of the group_edge pkey.

	}
	if incomingCursor != nil {

		if state == nil {
			params = append(params, incomingCursor.State)
		}
		params = append(params, incomingCursor.Position)
	}

	if limit != 0 {
		// Console API can select all group users in one request. Client/runtime calls will set a non-0 limit.
		params = append(params, limit+1)
		query += " LIMIT $" + strconv.Itoa(len(params))
	}

	{
		query, params = groupEdgesQueryMySQL(true, groupID, limit, state, incomingCursor)
	}
	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, runtime.ErrGroupNotFound
		}

		logger.Debug("Could not list users in group.", zap.Error(err), zap.String("group_id", groupID.String()))
		return nil, err
	}

	groupUsers := make([]*api.GroupUserList_GroupUser, 0, limit)
	var outgoingCursor string

	for rows.Next() {
		var id string
		var displayName sql.NullString
		var username sql.NullString
		var avatarURL sql.NullString
		var langTag sql.NullString
		var location sql.NullString
		var timezone sql.NullString
		var metadata []byte
		var apple sql.NullString
		var facebook sql.NullString
		var facebookInstantGame sql.NullString
		var google sql.NullString
		var gamecenter sql.NullString
		var steam sql.NullString
		var edgeCount int
		var createTime sql.NullTime
		var updateTime sql.NullTime
		var state sql.NullInt64
		var position sql.NullInt64

		if err := rows.Scan(&id, &username, &displayName, &avatarURL, &langTag, &location, &timezone, &metadata,
			&apple, &facebook, &facebookInstantGame, &google, &gamecenter, &steam, &edgeCount, &createTime, &updateTime, &state, &position); err != nil {
			_ = rows.Close()
			if errors.Is(err, sql.ErrNoRows) {
				return nil, runtime.ErrGroupNotFound
			}
			logger.Error("Could not parse rows when listing users in a group.", zap.Error(err), zap.String("group_id", groupID.String()))
			return nil, err
		}

		if limit != 0 && len(groupUsers) >= limit {
			cursorBuf := new(bytes.Buffer)
			if err := gob.NewEncoder(cursorBuf).Encode(&edgeListCursor{State: state.Int64, Position: position.Int64}); err != nil {
				_ = rows.Close()
				logger.Error("Error creating group user list cursor", zap.Error(err))
				return nil, err
			}
			outgoingCursor = base64.URLEncoding.EncodeToString(cursorBuf.Bytes())
			break
		}

		userID := uuid.Must(uuid.FromString(id))
		user := &api.User{
			Id:                    userID.String(),
			Username:              username.String,
			DisplayName:           displayName.String,
			AvatarUrl:             avatarURL.String,
			LangTag:               langTag.String,
			Location:              location.String,
			Timezone:              timezone.String,
			Metadata:              string(metadata),
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
		}

		groupUser := &api.GroupUserList_GroupUser{
			User: user,
			State: &wrapperspb.Int32Value{
				Value: int32(state.Int64),
			},
		}

		groupUsers = append(groupUsers, groupUser)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	statusRegistry.FillOnlineGroupUsers(groupUsers)

	return &api.GroupUserList{GroupUsers: groupUsers, Cursor: outgoingCursor}, nil
}

func ListUserGroups(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, limit int, state *wrapperspb.Int32Value, cursor string) (*api.UserGroupList, error) {
	var incomingCursor *edgeListCursor
	if cursor != "" {
		cb, err := base64.URLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, runtime.ErrUserGroupInvalidCursor
		}
		incomingCursor = &edgeListCursor{}
		if err := gob.NewDecoder(bytes.NewReader(cb)).Decode(incomingCursor); err != nil {
			return nil, runtime.ErrUserGroupInvalidCursor
		}

		// Cursor and filter mismatch. Perhaps the caller has sent an old cursor with a changed filter.
		if state != nil && int64(state.Value) != incomingCursor.State {
			return nil, runtime.ErrUserGroupInvalidCursor
		}
	}

	params := make([]interface{}, 0, 4)
	query := ""

	params = append(params, userID)
	if state != nil {
		// Assumes the state has already been validated before this function.

		params = append(params, state.Value)
	} else {
		// Hint for the query analyzer to ensure it performs and index scan on the appropriate range of the group_edge pkey.

	}
	if incomingCursor != nil {

		if state == nil {
			params = append(params, incomingCursor.State)
		}
		params = append(params, incomingCursor.Position)
	}

	if limit != 0 {
		// Console API can select all user groups in one request. Client/runtime calls will set a non-0 limit.
		params = append(params, limit+1)
		query += " LIMIT $" + strconv.Itoa(len(params))
	}

	{
		query, params = groupEdgesQueryMySQL(false, userID, limit, state, incomingCursor)
	}
	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, runtime.ErrGroupNotFound
		}
		logger.Debug("Could not list groups for a user.", zap.Error(err), zap.String("user_id", userID.String()))
		return nil, err
	}
	defer rows.Close()

	userGroups := make([]*api.UserGroupList_UserGroup, 0, limit)
	var outgoingCursor string

	for rows.Next() {
		var id string
		var creatorID sql.NullString
		var name sql.NullString
		var description sql.NullString
		var avatarURL sql.NullString
		var lang sql.NullString
		var metadata []byte
		var state sql.NullInt64
		var edgeCount sql.NullInt64
		var maxCount sql.NullInt64
		var createTime sql.NullTime
		var updateTime sql.NullTime
		var userState sql.NullInt64
		var userPosition sql.NullInt64

		if err := rows.Scan(&id, &creatorID, &name, &description, &avatarURL, &lang, &metadata, &state,
			&edgeCount, &maxCount, &createTime, &updateTime, &userState, &userPosition); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &api.UserGroupList{UserGroups: make([]*api.UserGroupList_UserGroup, 0)}, nil
			}
			logger.Error("Could not parse rows when listing groups for a user.", zap.Error(err), zap.String("user_id", userID.String()))
			return nil, err
		}

		if limit != 0 && len(userGroups) >= limit {
			cursorBuf := new(bytes.Buffer)
			if err := gob.NewEncoder(cursorBuf).Encode(&edgeListCursor{State: userState.Int64, Position: userPosition.Int64}); err != nil {
				logger.Error("Error creating group user list cursor", zap.Error(err))
				return nil, err
			}
			outgoingCursor = base64.URLEncoding.EncodeToString(cursorBuf.Bytes())
			break
		}

		open := true
		if state.Int64 == 1 {
			open = false
		}

		group := &api.Group{
			Id:          uuid.Must(uuid.FromString(id)).String(),
			CreatorId:   uuid.Must(uuid.FromString(creatorID.String)).String(),
			Name:        name.String,
			Description: description.String,
			AvatarUrl:   avatarURL.String,
			LangTag:     lang.String,
			Metadata:    string(metadata),
			Open:        &wrapperspb.BoolValue{Value: open},
			EdgeCount:   int32(edgeCount.Int64),
			MaxCount:    int32(maxCount.Int64),
			CreateTime:  &timestamppb.Timestamp{Seconds: createTime.Time.Unix()},
			UpdateTime:  &timestamppb.Timestamp{Seconds: updateTime.Time.Unix()},
		}

		userGroup := &api.UserGroupList_UserGroup{
			Group: group,
			State: &wrapperspb.Int32Value{
				Value: int32(userState.Int64),
			},
		}

		userGroups = append(userGroups, userGroup)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &api.UserGroupList{UserGroups: userGroups, Cursor: outgoingCursor}, nil
}

func GetGroups(ctx context.Context, logger *zap.Logger, db *sql.DB, ids []string) ([]*api.Group, error) {
	if len(ids) == 0 {
		return make([]*api.Group, 0), nil
	}

	{
		canonical, err := canonicalUserIDsMySQL(ids)
		if err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, mysqlGroupSelect+" WHERE "+mysqlActiveGroup+" AND id IN ("+mysqlMarks(len(canonical))+")", mysqlStringArgs(canonical)...)
		if err != nil {
			return nil, err
		}
		groups, _, err := groupConvertRows(rows, len(ids))
		return groups, err
	}

}

func ListGroups(ctx context.Context, logger *zap.Logger, db *sql.DB, name, langTag string, open *bool, edgeCount, limit int, cursorStr string) (*api.GroupList, error) {
	if name != "" && (langTag != "" || open != nil || edgeCount > -1) {
		return nil, StatusError(codes.InvalidArgument, "name filter cannot be combined with any other filter", nil)
	}

	if name != "" {
		name = string(bytes.TrimLeft([]byte(name), "% "))
	}

	var cursor *groupListCursor
	if cursorStr != "" {
		cursor = &groupListCursor{}
		cb, err := base64.RawURLEncoding.DecodeString(cursorStr)
		if err != nil {
			logger.Warn("Could not base64 decode group listing cursor.", zap.String("cursor", cursorStr))
			return nil, status.Error(codes.InvalidArgument, "Malformed cursor was used.")
		}
		if err = gob.NewDecoder(bytes.NewReader(cb)).Decode(cursor); err != nil {
			logger.Warn("Could not decode group listing cursor.", zap.String("cursor", cursorStr))
			return nil, status.Error(codes.InvalidArgument, "Malformed cursor was used.")
		}
	}

	groupList := &api.GroupList{Groups: make([]*api.Group, 0)}
	query, params := listGroupsQueryMySQL(name, langTag, open, edgeCount, limit, cursor)
	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return groupList, nil
		}
		logger.Error("Could not list groups.", zap.Error(err), zap.String("name", name))
		return nil, err
	}

	// Rows closed in groupConvertRows()
	groups, newCursorStr, err := groupConvertRows(rows, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return groupList, nil
		}
		logger.Error("Could not list groups.", zap.Error(err), zap.String("name", name))
		return nil, err
	}

	groupList.Groups = groups
	groupList.Cursor = newCursorStr

	return groupList, nil
}

type groupSqlStruct struct {
	id          string
	creatorID   sql.NullString
	name        sql.NullString
	description sql.NullString
	avatarURL   sql.NullString
	lang        sql.NullString
	metadata    []byte
	state       sql.NullInt64
	edgeCount   sql.NullInt64
	maxCount    sql.NullInt64
	createTime  sql.NullTime
	updateTime  sql.NullTime
}

func sqlMapper(row *groupSqlStruct) (*api.Group, *time.Time) {
	open := true
	if row.state.Int64 == 1 {
		open = false
	}
	return &api.Group{
		Id:          uuid.Must(uuid.FromString(row.id)).String(),
		CreatorId:   uuid.Must(uuid.FromString(row.creatorID.String)).String(),
		Name:        row.name.String,
		Description: row.description.String,
		AvatarUrl:   row.avatarURL.String,
		LangTag:     row.lang.String,
		Metadata:    string(row.metadata),
		Open:        &wrapperspb.BoolValue{Value: open},
		EdgeCount:   int32(row.edgeCount.Int64),
		MaxCount:    int32(row.maxCount.Int64),
		CreateTime:  &timestamppb.Timestamp{Seconds: row.createTime.Time.Unix()},
		UpdateTime:  &timestamppb.Timestamp{Seconds: row.updateTime.Time.Unix()},
	}, &row.updateTime.Time
}

func convertToGroup(rows *sql.Rows) (*api.Group, *time.Time, error) {
	s := groupSqlStruct{}
	groupStruct, fields := &s, []any{&s.id, &s.creatorID, &s.name, &s.description, &s.avatarURL, &s.state, &s.edgeCount, &s.lang,
		&s.maxCount, &s.metadata, &s.createTime, &s.updateTime}
	if err := rows.Scan(fields...); err != nil {
		return nil, nil, err
	}
	group, updateTime := sqlMapper(groupStruct)

	return group, updateTime, nil
}

func groupConvertRows(rows *sql.Rows, limit int) ([]*api.Group, string, error) {
	defer rows.Close()

	groups := make([]*api.Group, 0, limit+1)
	updateTimes := make([]*time.Time, 0, limit+1)
	var updateTime *time.Time
	for rows.Next() {
		var group *api.Group
		var err error
		if group, updateTime, err = convertToGroup(rows); err != nil {
			return nil, "", err
		} else {
			groups = append(groups, group)
			updateTimes = append(updateTimes, updateTime)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	outGroups := groups[:int(math.Min(float64(len(groups)), float64(limit)))]

	var cursor string
	cursorBuf := new(bytes.Buffer)
	if len(groups) > limit {
		lastGroup := outGroups[len(outGroups)-1]
		newCursor := &groupListCursor{
			ID:         uuid.Must(uuid.FromString(lastGroup.Id)),
			EdgeCount:  lastGroup.EdgeCount,
			Lang:       lastGroup.LangTag,
			Name:       lastGroup.Name,
			Open:       lastGroup.Open.Value,
			UpdateTime: updateTimes[len(outGroups)-1].UnixNano(),
		}

		if err := gob.NewEncoder(cursorBuf).Encode(newCursor); err != nil {
			return nil, "", err
		}

		cursor = base64.RawURLEncoding.EncodeToString(cursorBuf.Bytes())
	}

	return outGroups, cursor, nil
}

func groupCheckUserPermission(ctx context.Context, logger *zap.Logger, db *sql.DB, groupID, userID uuid.UUID, state int) (bool, error) {
	query := "SELECT state FROM group_edge WHERE source_id = ? AND destination_id = ?"
	var dbState int
	if err := db.QueryRowContext(ctx, query, groupID, userID).Scan(&dbState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		logger.Error("Could not look up user state with group.", zap.Error(err), zap.String("group_id", groupID.String()), zap.String("user_id", userID.String()))
		return false, err
	}
	return dbState <= state, nil
}

func GetRandomGroups(ctx context.Context, logger *zap.Logger, db *sql.DB, count int) ([]*api.Group, error) {
	if count == 0 {
		return []*api.Group{}, nil
	}

	query := mysqlGroupSelect + " WHERE id > ? ORDER BY id LIMIT ?"
	rows, err := db.QueryContext(ctx, query, uuid.Must(uuid.NewV4()).String(), count)
	if err != nil {
		logger.Error("Error retrieving random groups.", zap.Error(err))
		return nil, err
	}
	groups := make([]*api.Group, 0, count)
	for rows.Next() {
		group, _, err := convertToGroup(rows)
		if err != nil {
			_ = rows.Close()
			logger.Error("Error retrieving random groups.", zap.Error(err))
			return nil, err
		}
		groups = append(groups, group)
	}
	_ = rows.Close()

	if len(groups) < count {
		// Need more groups.
		rows, err = db.QueryContext(ctx, query, uuid.Nil.String(), count)
		if err != nil {
			logger.Error("Error retrieving random groups.", zap.Error(err))
			return nil, err
		}
		for rows.Next() {
			group, _, err := convertToGroup(rows)
			if err != nil {
				_ = rows.Close()
				logger.Error("Error retrieving random groups.", zap.Error(err))
				return nil, err
			}
			var found bool
			for _, existing := range groups {
				if existing.Id == group.Id {
					found = true
					break
				}
			}
			if !found {
				groups = append(groups, group)
			}
			if len(groups) >= count {
				break
			}
		}
		_ = rows.Close()
	}

	return groups, nil
}

func getGroup(ctx context.Context, logger *zap.Logger, db *sql.DB, groupID uuid.UUID) (*api.Group, error) {
	query := mysqlGroupSelect + " WHERE id = ?"

	s := groupSqlStruct{}
	groupStruct, fields := &s, []any{&s.id, &s.creatorID, &s.name, &s.description, &s.avatarURL, &s.state, &s.edgeCount, &s.lang,
		&s.maxCount, &s.metadata, &s.createTime, &s.updateTime}

	if err := db.QueryRowContext(ctx, query, groupID).Scan(fields...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, runtime.ErrGroupNotFound
		}
		logger.Error("Error retrieving group.", zap.Error(err))
		return nil, err
	}

	group, _ := sqlMapper(groupStruct)

	return group, nil
}
