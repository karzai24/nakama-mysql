// Modified by nakama-mysql contributors: native MySQL identity linking.
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

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama/v3/social"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func LinkApple(ctx context.Context, logger *zap.Logger, db *sql.DB, config Config, socialClient *social.Client, userID uuid.UUID, token string) error {
	if config.GetSocial().Apple.BundleId == "" {
		return status.Error(codes.FailedPrecondition, "Apple authentication is not configured.")
	}

	if token == "" {
		return status.Error(codes.InvalidArgument, "Apple ID token is required.")
	}

	profile, err := socialClient.CheckAppleToken(ctx, config.GetSocial().Apple.BundleId, token)
	if err != nil {
		logger.Info("Could not authenticate Apple profile.", zap.Error(err))
		return status.Error(codes.Unauthenticated, "Could not authenticate Apple profile.")
	}

	return linkProviderMySQL(ctx, logger, db, userID, identityProviderProfile{column: "apple_id", id: profile.ID, email: profile.Email})

}

func LinkCustom(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, customID string) error {
	if customID == "" {
		return status.Error(codes.InvalidArgument, "Custom ID is required.")
	} else if invalidCharsRegex.MatchString(customID) {
		return status.Error(codes.InvalidArgument, "Invalid custom ID, no spaces or control characters allowed.")
	} else if len(customID) < 6 || len(customID) > 128 {
		return status.Error(codes.InvalidArgument, "Invalid custom ID, must be 6-128 bytes.")
	}

	return linkUserIdentifierMySQL(ctx, logger, db, userID, "custom_id", customID, nil)

}

func LinkDevice(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, deviceID string) error {
	if deviceID == "" {
		return status.Error(codes.InvalidArgument, "Device ID is required.")
	} else if invalidCharsRegex.MatchString(deviceID) {
		return status.Error(codes.InvalidArgument, "Device ID invalid, no spaces or control characters allowed.")
	} else if len(deviceID) < 10 || len(deviceID) > 128 {
		return status.Error(codes.InvalidArgument, "Device ID invalid, must be 10-128 bytes.")
	}

	return linkDeviceMySQL(ctx, logger, db, userID, deviceID)

}

func LinkEmail(ctx context.Context, logger *zap.Logger, db *sql.DB, userID uuid.UUID, email, password string) error {
	if email == "" || password == "" {
		return status.Error(codes.InvalidArgument, "Email address and password is required.")
	} else if invalidCharsRegex.MatchString(email) {
		return status.Error(codes.InvalidArgument, "Invalid email address, no spaces or control characters allowed.")
	} else if len(password) < 8 {
		return status.Error(codes.InvalidArgument, "Password must be at least 8 characters long.")
	} else if !emailRegex.MatchString(email) {
		return status.Error(codes.InvalidArgument, "Invalid email address format.")
	} else if len(email) < 10 || len(email) > 255 {
		return status.Error(codes.InvalidArgument, "Invalid email address, must be 10-255 bytes.")
	}

	return linkEmailMySQL(ctx, logger, db, userID, email, password)

}

func LinkFacebook(ctx context.Context, logger *zap.Logger, db *sql.DB, socialClient *social.Client, tracker Tracker, router MessageRouter, userID uuid.UUID, username, appId, token string, sync bool) error {
	if token == "" {
		return status.Error(codes.InvalidArgument, "Facebook access token is required.")
	}

	var facebookProfile *social.FacebookProfile
	var err error
	var importFriendsPossible bool

	facebookProfile, err = socialClient.CheckFacebookLimitedLoginToken(ctx, appId, token)
	if err != nil {
		facebookProfile, err = socialClient.GetFacebookProfile(ctx, token)
		if err != nil {
			logger.Info("Could not authenticate Facebook profile.", zap.Error(err))
			return status.Error(codes.Unauthenticated, "Could not authenticate Facebook profile.")
		}
		importFriendsPossible = true
	}

	{
		if err := linkProviderMySQL(ctx, logger, db, userID, identityProviderProfile{column: "facebook_id", id: facebookProfile.ID, email: facebookProfile.Email, displayName: facebookProfile.Name, avatarURL: facebookProfile.Picture.Data.Url}); err != nil {
			return err
		}
		if sync && importFriendsPossible {
			_ = importFacebookFriends(ctx, logger, db, tracker, router, socialClient, userID, username, token, false)
		}
		return nil
	}

}

func LinkFacebookInstantGame(ctx context.Context, logger *zap.Logger, db *sql.DB, config Config, socialClient *social.Client, userID uuid.UUID, signedPlayerInfo string) error {
	if signedPlayerInfo == "" {
		return status.Error(codes.InvalidArgument, "Signed Player Info for a Facebook Instant Game is required.")
	}

	facebookInstantGameID, err := socialClient.ExtractFacebookInstantGameID(signedPlayerInfo, config.GetSocial().FacebookInstantGame.AppSecret)
	if err != nil {
		logger.Info("Could not authenticate Facebook Instant Game profile.", zap.Error(err))
		return status.Error(codes.Unauthenticated, "Could not authenticate Facebook Instant Game profile.")
	}

	return linkProviderMySQL(ctx, logger, db, userID, identityProviderProfile{column: "facebook_instant_game_id", id: facebookInstantGameID})

}

func LinkGameCenter(ctx context.Context, logger *zap.Logger, db *sql.DB, socialClient *social.Client, userID uuid.UUID, playerID string, bundleID string, timestamp int64, salt string, signature string, publicKeyURL string) error {
	if bundleID == "" {
		return status.Error(codes.InvalidArgument, "GameCenter bundle ID is required.")
	} else if playerID == "" {
		return status.Error(codes.InvalidArgument, "GameCenter player ID is required.")
	} else if publicKeyURL == "" {
		return status.Error(codes.InvalidArgument, "GameCenter public key URL is required.")
	} else if salt == "" {
		return status.Error(codes.InvalidArgument, "GameCenter salt is required.")
	} else if signature == "" {
		return status.Error(codes.InvalidArgument, "GameCenter signature is required.")
	} else if timestamp == 0 {
		return status.Error(codes.InvalidArgument, "GameCenter timestamp is required.")
	}

	valid, err := socialClient.CheckGameCenterID(ctx, playerID, bundleID, timestamp, salt, signature, publicKeyURL)
	if !valid || err != nil {
		logger.Info("Could not authenticate GameCenter profile.", zap.Error(err), zap.Bool("valid", valid))
		return status.Error(codes.Unauthenticated, "Could not authenticate GameCenter profile.")
	}

	return linkProviderMySQL(ctx, logger, db, userID, identityProviderProfile{column: "gamecenter_id", id: playerID})

}

func LinkGoogle(ctx context.Context, logger *zap.Logger, db *sql.DB, socialClient *social.Client, userID uuid.UUID, idToken string) error {
	if idToken == "" {
		return status.Error(codes.InvalidArgument, "Google access token is required.")
	}

	googleProfile, err := socialClient.CheckGoogleToken(ctx, idToken)
	if err != nil {
		logger.Info("Could not authenticate Google profile.", zap.Error(err))
		return status.Error(codes.Unauthenticated, "Could not authenticate Google profile.")
	}

	return linkProviderMySQL(ctx, logger, db, userID, identityGoogleProfile(googleProfile))

}

func LinkSteam(ctx context.Context, logger *zap.Logger, db *sql.DB, config Config, socialClient *social.Client, tracker Tracker, router MessageRouter, userID uuid.UUID, username, token string, sync bool) error {
	if config.GetSocial().Steam.PublisherKey == "" || config.GetSocial().Steam.AppID == 0 {
		return status.Error(codes.FailedPrecondition, "Steam authentication is not configured.")
	}

	if token == "" {
		return status.Error(codes.InvalidArgument, "Steam access token is required.")
	}

	steamProfile, err := socialClient.GetSteamProfile(ctx, config.GetSocial().Steam.PublisherKey, config.GetSocial().Steam.AppID, token)
	if err != nil {
		logger.Info("Could not authenticate Steam profile.", zap.Error(err))
		return status.Error(codes.Unauthenticated, "Could not authenticate Steam profile.")
	}

	{
		if err := linkProviderMySQL(ctx, logger, db, userID, identityProviderProfile{column: "steam_id", id: strconv.FormatUint(steamProfile.SteamID, 10)}); err != nil {
			return err
		}
		if sync {
			_ = importSteamFriends(ctx, logger, db, tracker, router, socialClient, userID, username, config.GetSocial().Steam.PublisherKey, strconv.FormatUint(steamProfile.SteamID, 10), false)
		}
		return nil
	}

}
