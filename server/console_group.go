// Copyright 2022 The Nakama Authors
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

	"strings"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"

	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/console"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type consoleGroupCursor struct {
	ID   uuid.UUID
	Name string
}

func (s *ConsoleServer) ListGroups(ctx context.Context, in *console.ListGroupsRequest) (*console.GroupList, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	const defaultLimit = 50

	// Validate cursor, if provided.
	var cursor *consoleGroupCursor
	if in.Cursor != "" {
		cb, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil {
			logger.Error("Error decoding group list cursor.", zap.String("cursor", in.Cursor), zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to decode group list request cursor.")
		}
		cursor = &consoleGroupCursor{}
		if err := gob.NewDecoder(bytes.NewReader(cb)).Decode(&cursor); err != nil {
			logger.Error("Error decoding account list cursor.", zap.String("cursor", in.Cursor), zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to decode group list request cursor.")
		}
	}

	query, params, limit := consoleGroupsQueryMySQL(in.Filter, cursor)

	rows, err := s.db.QueryContext(ctx, query, params...)
	if err != nil {
		logger.Error("Error querying groups.", zap.Any("in", in), zap.Error(err))
		return nil, status.Error(codes.Internal, "An error occurred while trying to list groups.")
	}

	groups := make([]*api.Group, 0, defaultLimit)
	var nextCursor *consoleGroupCursor
	var previousGroup *api.Group

	for rows.Next() {
		group, _, err := convertToGroup(rows)
		if err != nil {
			_ = rows.Close()
			logger.Error("Error scanning groups.", zap.Any("in", in), zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to list groups.")
		}
		// checks limit before append for the use case where (last page == limit) => null cursor
		if limit > 0 && len(groups) >= limit {
			nextCursor = &consoleGroupCursor{
				ID:   uuid.FromStringOrNil(previousGroup.Id),
				Name: previousGroup.Name,
			}
			break
		}
		groups = append(groups, group)
		previousGroup = group
	}
	_ = rows.Close()

	if err := rows.Err(); err != nil {
		return nil, err
	}

	response := &console.GroupList{
		Groups:     groups,
		TotalCount: countDatabase(ctx, logger, s.db, "groups"),
	}

	if nextCursor != nil {
		cursorBuf := &bytes.Buffer{}
		if err := gob.NewEncoder(cursorBuf).Encode(nextCursor); err != nil {
			logger.Error("Error encoding groups cursor.", zap.Any("in", in), zap.Error(err))
			return nil, status.Error(codes.Internal, "An error occurred while trying to list groups.")
		}
		response.NextCursor = base64.RawURLEncoding.EncodeToString(cursorBuf.Bytes())
	}

	return response, nil
}

func (s *ConsoleServer) DeleteGroup(ctx context.Context, in *console.DeleteGroupRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	groupID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid group ID.")
	}

	if err = DeleteGroup(ctx, logger, s.db, s.tracker, groupID, uuid.Nil); err != nil {
		// Error already logged in function above.
		return nil, status.Error(codes.Internal, "An error occurred while trying to delete the user.")
	}

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) GetGroup(ctx context.Context, in *console.GroupId) (*api.Group, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	groupID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid group ID.")
	}

	group, err := getGroup(ctx, logger, s.db, groupID)
	if err != nil {
		if err == runtime.ErrGroupNotFound {
			return nil, status.Error(codes.NotFound, "Group not found.")
		}
		return nil, status.Error(codes.Internal, "An error occurred while trying to retrieve group.")
	}

	return group, nil
}

func (s *ConsoleServer) ExportGroup(ctx context.Context, in *console.GroupId) (*console.GroupExport, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	groupID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid group ID.")
	}
	if groupID == uuid.Nil {
		return nil, status.Error(codes.InvalidArgument, "Cannot export the group.")
	}

	group, err := getGroup(ctx, logger, s.db, groupID)
	if err != nil {
		if err == runtime.ErrGroupNotFound {
			return nil, status.Error(codes.NotFound, "Group not found.")
		}
		return nil, status.Error(codes.Internal, "An error occurred while trying to export group data.")
	}

	users, err := ListGroupUsers(ctx, logger, s.db, s.statusRegistry, groupID, 0, nil, "")
	if err != nil {
		return nil, status.Error(codes.Internal, "An error occurred while trying to export group members.")
	}

	return &console.GroupExport{
		Group:   group,
		Members: users.GroupUsers,
	}, nil
}

func (s *ConsoleServer) UpdateGroup(ctx context.Context, in *console.UpdateGroupRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	groupID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid group ID.")
	}

	var maxCount int
	if in.MaxCount != nil {
		maxCount = int(in.MaxCount.Value)
	}

	err = UpdateGroup(ctx, logger, s.db, groupID, uuid.Nil, uuid.Nil, in.Name, in.LangTag, in.Description, in.AvatarUrl, in.Metadata, in.Open, maxCount)
	if err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) GetMembers(ctx context.Context, in *console.GroupId) (*api.GroupUserList, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	groupID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid group ID.")
	}

	users, err := ListGroupUsers(ctx, logger, s.db, s.statusRegistry, groupID, 0, nil, "")
	if err != nil {
		return nil, status.Error(codes.Internal, "An error occurred while trying to list group members.")
	}

	return users, nil
}

func (s *ConsoleServer) DemoteGroupMember(ctx context.Context, in *console.UpdateGroupUserStateRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	groupID, err := uuid.FromString(in.GroupId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid group ID.")
	}

	{
		if err := mutateGroupMySQL(ctx, logger, s.db, s.tracker, s.router, nil, uuid.Nil, groupID, []uuid.UUID{userID}, "", "console_demote", true); err != nil {
			if err == ErrEmptyMemberDemote || err == ErrEmptyMemberPromote {
				return nil, status.Error(codes.FailedPrecondition, "Cannot demote user in the group.")
			}
			return nil, status.Error(codes.Internal, "Could not demote group member.")
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) PromoteGroupMember(ctx context.Context, in *console.UpdateGroupUserStateRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	groupID, err := uuid.FromString(in.GroupId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid group ID.")
	}

	{
		if err := mutateGroupMySQL(ctx, logger, s.db, s.tracker, s.router, nil, uuid.Nil, groupID, []uuid.UUID{userID}, "", "console_promote", true); err != nil {
			if err == ErrEmptyMemberDemote || err == ErrEmptyMemberPromote {
				return nil, status.Error(codes.FailedPrecondition, "Cannot promote user in the group.")
			}
			return nil, status.Error(codes.Internal, "Could not promote group member.")
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) AddGroupUsers(ctx context.Context, in *console.AddGroupUsersRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	groupUid, err := uuid.FromString(in.GroupId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "Invalid group ID format.")
	}
	ids := strings.Split(in.Ids, ",")
	uuids := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		id := strings.TrimSpace(id)
		uid, err := uuid.FromString(id)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "Invalid user ID format: "+id)
		}
		uuids = append(uuids, uid)
	}

	if in.JoinRequest {
		for _, uid := range uuids {
			// Look up the username, and implicitly if this user exists.
			var username sql.NullString
			query := "SELECT username FROM users WHERE id = ?"
			if err = s.db.QueryRowContext(ctx, query, uid).Scan(&username); err != nil {
				if err == sql.ErrNoRows {
					return nil, status.Error(codes.InvalidArgument, "User not found: "+uid.String()+". Refresh the page to see any updates.")
				}
				logger.Debug("Could not retrieve username to join user to group.", zap.Error(err), zap.String("user_id", uid.String()))
				return nil, status.Error(codes.Internal, "An error occurred while trying to join the user to the group. Refresh the page to see any updates.")
			}
			if err = JoinGroup(ctx, logger, s.db, s.tracker, s.router, groupUid, uid, username.String); err != nil {
				return nil, status.Error(codes.Internal, "An error occurred while trying to join an user to the group, refresh the page: "+err.Error()+". Refresh the page to see any updates.")
			}
		}
	} else {
		if err = AddGroupUsers(ctx, logger, s.db, s.tracker, s.router, uuid.Nil, groupUid, uuids); err != nil {
			return nil, status.Error(codes.Internal, "An error occurred while trying to add the users: "+err.Error())
		}
	}
	return &emptypb.Empty{}, nil
}
