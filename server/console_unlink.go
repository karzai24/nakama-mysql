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

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/console"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (s *ConsoleServer) UnlinkApple(ctx context.Context, in *console.AccountId) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	{
		if err := unlinkIdentifierMySQL(ctx, logger, s.db, userID, "apple_id", ""); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) UnlinkCustom(ctx context.Context, in *console.AccountId) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	{
		if err := unlinkIdentifierMySQL(ctx, logger, s.db, userID, "custom_id", ""); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) Unlink(ctx context.Context, in *console.UnlinkRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	if err := Unlink(ctx, logger, s.db, userID, in.Provider); err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (s *ConsoleServer) UnlinkDevice(ctx context.Context, in *console.UnlinkDeviceRequest) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}
	if in.DeviceId == "" {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid device ID.")
	}

	{
		if err := unlinkIdentifierMySQL(ctx, logger, s.db, userID, "device", in.DeviceId); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) UnlinkEmail(ctx context.Context, in *console.AccountId) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	{
		if err := unlinkIdentifierMySQL(ctx, logger, s.db, userID, "email", ""); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) UnlinkFacebook(ctx context.Context, in *console.AccountId) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	{
		if err := unlinkIdentifierMySQL(ctx, logger, s.db, userID, "facebook_id", ""); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) UnlinkFacebookInstantGame(ctx context.Context, in *console.AccountId) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	{
		if err := unlinkIdentifierMySQL(ctx, logger, s.db, userID, "facebook_instant_game_id", ""); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) UnlinkGameCenter(ctx context.Context, in *console.AccountId) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	{
		if err := unlinkIdentifierMySQL(ctx, logger, s.db, userID, "gamecenter_id", ""); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) UnlinkGoogle(ctx context.Context, in *console.AccountId) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	{
		if err := unlinkIdentifierMySQL(ctx, logger, s.db, userID, "google_id", ""); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}

}

func (s *ConsoleServer) UnlinkSteam(ctx context.Context, in *console.AccountId) (*emptypb.Empty, error) {
	logger, _ := LoggerWithTraceId(ctx, s.logger)
	userID, err := uuid.FromString(in.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Requires a valid user ID.")
	}

	{
		if err := unlinkIdentifierMySQL(ctx, logger, s.db, userID, "steam_id", ""); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}

}
