// Modified by nakama-mysql contributors: native MySQL identity unlinking.
// Copyright 2020 The Nakama Authors
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
	"strconv"
	"strings"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/social"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func UnlinkApple(ctx context.Context, logger *zap.Logger, db *sql.DB, config Config, socialClient *social.Client, id uuid.UUID, token string) error {
	params := []any{id}

	if token != "" {
		profile, err := socialClient.CheckAppleToken(ctx, config.GetSocial().Apple.BundleId, token)
		if err != nil {
			logger.Info("Could not authenticate Apple profile.", zap.Error(err))
			return status.Error(codes.Unauthenticated, "Could not authenticate Apple profile.")
		}
		params = append(params, profile.ID)
	}

	{
		identifier := ""
		if len(params) > 1 {
			identifier = params[1].(string)
		}
		return unlinkIdentifierMySQL(ctx, logger, db, id, "apple_id", identifier)
	}

}

func UnlinkCustom(ctx context.Context, logger *zap.Logger, db *sql.DB, id uuid.UUID, customID string) error {

	return unlinkIdentifierMySQL(ctx, logger, db, id, "custom_id", customID)

}

func UnlinkDevice(ctx context.Context, logger *zap.Logger, db *sql.DB, id uuid.UUID, deviceID string) error {
	if deviceID == "" {
		return status.Error(codes.InvalidArgument, "A device ID must be supplied.")
	}

	return unlinkIdentifierMySQL(ctx, logger, db, id, "device", deviceID)

}

func UnlinkEmail(ctx context.Context, logger *zap.Logger, db *sql.DB, id uuid.UUID, email string) error {

	return unlinkIdentifierMySQL(ctx, logger, db, id, "email", strings.ToLower(email))

}

func UnlinkFacebook(ctx context.Context, logger *zap.Logger, db *sql.DB, socialClient *social.Client, appId string, id uuid.UUID, token string) error {
	params := []any{id}

	if token != "" {
		facebookProfile, err := socialClient.CheckFacebookLimitedLoginToken(ctx, appId, token)
		if err != nil {
			facebookProfile, err = socialClient.GetFacebookProfile(ctx, token)
			if err != nil {
				logger.Info("Could not authenticate Facebook profile.", zap.Error(err))
				return status.Error(codes.Unauthenticated, "Could not authenticate Facebook profile.")
			}
		}
		params = append(params, facebookProfile.ID)
	}

	{
		identifier := ""
		if len(params) > 1 {
			identifier = params[1].(string)
		}
		return unlinkIdentifierMySQL(ctx, logger, db, id, "facebook_id", identifier)
	}

}

func UnlinkFacebookInstantGame(ctx context.Context, logger *zap.Logger, db *sql.DB, config Config, socialClient *social.Client, id uuid.UUID, signedPlayerInfo string) error {
	params := []any{id}

	if signedPlayerInfo != "" {
		facebookInstantGameID, err := socialClient.ExtractFacebookInstantGameID(signedPlayerInfo, config.GetSocial().FacebookInstantGame.AppSecret)
		if err != nil {
			logger.Info("Could not authenticate Facebook Instant Game profile.", zap.Error(err))
			return status.Error(codes.Unauthenticated, "Could not authenticate Facebook Instant Game profile.")
		}
		params = append(params, facebookInstantGameID)
	}

	{
		identifier := ""
		if len(params) > 1 {
			identifier = params[1].(string)
		}
		return unlinkIdentifierMySQL(ctx, logger, db, id, "facebook_instant_game_id", identifier)
	}

}

func UnlinkGameCenter(ctx context.Context, logger *zap.Logger, db *sql.DB, socialClient *social.Client, id uuid.UUID, playerID string, bundleID string, timestamp int64, salt string, signature string, publicKeyURL string) error {
	params := []any{id}

	if bundleID != "" && playerID != "" && publicKeyURL != "" && salt != "" && signature != "" && timestamp != 0 {
		valid, err := socialClient.CheckGameCenterID(ctx, playerID, bundleID, timestamp, salt, signature, publicKeyURL)
		if !valid || err != nil {
			logger.Info("Could not authenticate GameCenter profile.", zap.Error(err), zap.Bool("valid", valid))
			return status.Error(codes.Unauthenticated, "Could not authenticate GameCenter profile.")
		}
		params = append(params, playerID)
	}

	{
		identifier := ""
		if len(params) > 1 {
			identifier = params[1].(string)
		}
		return unlinkIdentifierMySQL(ctx, logger, db, id, "gamecenter_id", identifier)
	}

}

func UnlinkGoogle(ctx context.Context, logger *zap.Logger, db *sql.DB, socialClient *social.Client, id uuid.UUID, token string) error {

	params := []any{id}

	if token != "" {
		googleProfile, err := socialClient.CheckGoogleToken(ctx, token)
		if err != nil {
			logger.Info("Could not authenticate Google profile.", zap.Error(err))
			return status.Error(codes.Unauthenticated, "Could not authenticate Google profile.")
		}
		params = append(params, googleProfile.GetGoogleId())

		err = RemapGoogleId(ctx, logger, db, googleProfile)
		if err != nil {
			logger.Error("Could not remap Google ID.", zap.Error(err), zap.String("googleId", googleProfile.GetGoogleId()),
				zap.String("originalGoogleId", googleProfile.GetOriginalGoogleId()), zap.Any("input", token))
			return status.Error(codes.Internal, "Error while trying to unlink Google ID.")
		}
	}

	{
		identifier := ""
		if len(params) > 1 {
			identifier = params[1].(string)
		}
		return unlinkIdentifierMySQL(ctx, logger, db, id, "google_id", identifier)
	}

}

func UnlinkSteam(ctx context.Context, logger *zap.Logger, db *sql.DB, config Config, socialClient *social.Client, id uuid.UUID, token string) error {
	params := []any{id}

	if token != "" {
		steamProfile, err := socialClient.GetSteamProfile(ctx, config.GetSocial().Steam.PublisherKey, config.GetSocial().Steam.AppID, token)
		if err != nil {
			logger.Info("Could not authenticate Steam profile.", zap.Error(err))
			return status.Error(codes.Unauthenticated, "Could not authenticate Steam profile.")
		}
		params = append(params, strconv.FormatUint(steamProfile.SteamID, 10))
	}

	{
		identifier := ""
		if len(params) > 1 {
			identifier = params[1].(string)
		}
		return unlinkIdentifierMySQL(ctx, logger, db, id, "steam_id", identifier)
	}

}
