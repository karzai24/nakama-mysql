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
	"math"
	"strconv"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/social"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var bcryptHashCost = int(math.Max(float64(bcrypt.DefaultCost), 13))

func AuthenticateApple(ctx context.Context, logger *zap.Logger, db *sql.DB, client *social.Client, bundleId, token, username string, create bool) (string, string, bool, error) {
	profile, err := client.CheckAppleToken(ctx, bundleId, token)
	if err != nil {
		logger.Info("Could not authenticate Apple profile.", zap.Error(err))
		return "", "", false, status.Error(codes.Unauthenticated, "Could not authenticate Apple profile.")
	}

	return authenticateProviderMySQL(ctx, logger, db, identityProviderProfile{column: "apple_id", id: profile.ID, email: profile.Email}, username, create)

}

func AuthenticateCustom(ctx context.Context, logger *zap.Logger, db *sql.DB, customID, username string, create bool) (string, string, bool, error) {

	return authenticateCustomMySQL(ctx, logger, db, customID, username, create)

}

func AuthenticateDevice(ctx context.Context, logger *zap.Logger, db *sql.DB, deviceID, username string, create bool) (string, string, bool, error) {

	return authenticateDeviceMySQL(ctx, logger, db, deviceID, username, create)

}

func AuthenticateEmail(ctx context.Context, logger *zap.Logger, db *sql.DB, email, password, username string, create bool) (string, string, bool, error) {

	return authenticateEmailMySQL(ctx, logger, db, email, password, username, create)

}

func AuthenticateUsername(ctx context.Context, logger *zap.Logger, db *sql.DB, username, password string) (string, error) {

	return authenticateUsernameMySQL(ctx, logger, db, username, password)

}

func AuthenticateFacebook(ctx context.Context, logger *zap.Logger, db *sql.DB, client *social.Client, appId, accessToken, username string, create bool) (string, string, bool, error) {
	var facebookProfile *social.FacebookProfile
	var err error

	// Try Facebook Limited Login first.
	facebookProfile, err = client.CheckFacebookLimitedLoginToken(ctx, appId, accessToken)
	if err != nil {
		// If that failed try standard Facebook auth.
		facebookProfile, err = client.GetFacebookProfile(ctx, accessToken)
		if err != nil {
			logger.Info("Could not authenticate Facebook profile.", zap.Error(err))
			return "", "", false, status.Error(codes.Unauthenticated, "Could not authenticate Facebook profile.")
		}
	}

	return authenticateProviderMySQL(ctx, logger, db, identityProviderProfile{column: "facebook_id", id: facebookProfile.ID, email: facebookProfile.Email, displayName: facebookProfile.Name, avatarURL: facebookProfile.Picture.Data.Url}, username, create)

}

func AuthenticateFacebookInstantGame(ctx context.Context, logger *zap.Logger, db *sql.DB, client *social.Client, appSecret string, signedPlayerInfo string, username string, create bool) (string, string, bool, error) {
	facebookInstantGameID, err := client.ExtractFacebookInstantGameID(signedPlayerInfo, appSecret)
	if err != nil {
		logger.Error("Error extracting the Facebook Instant Game player ID or validating the signature", zap.Error(err), zap.String("signedPlayerInfo", signedPlayerInfo))
		return "", "", false, status.Error(codes.DataLoss, "Error extracting the Facebook Instant Game player ID or validating the signature")
	}

	return authenticateProviderMySQL(ctx, logger, db, identityProviderProfile{column: "facebook_instant_game_id", id: facebookInstantGameID}, username, create)

}

func AuthenticateGameCenter(ctx context.Context, logger *zap.Logger, db *sql.DB, client *social.Client, playerID, bundleID string, timestamp int64, salt, signature, publicKeyUrl, username string, create bool) (string, string, bool, error) {
	valid, err := client.CheckGameCenterID(ctx, playerID, bundleID, timestamp, salt, signature, publicKeyUrl)
	if !valid || err != nil {
		logger.Info("Could not authenticate GameCenter profile.", zap.Error(err), zap.Bool("valid", valid))
		return "", "", false, status.Error(codes.Unauthenticated, "Could not authenticate GameCenter profile.")
	}

	return authenticateProviderMySQL(ctx, logger, db, identityProviderProfile{column: "gamecenter_id", id: playerID}, username, create)

}

func RemapGoogleId(ctx context.Context, logger *zap.Logger, db *sql.DB, googleProfile social.GoogleProfile) error {

	return remapGoogleIDMySQL(ctx, db, googleProfile.GetGoogleId(), googleProfile.GetOriginalGoogleId())

}

func AuthenticateGoogle(ctx context.Context, logger *zap.Logger, db *sql.DB, client *social.Client, idToken, username string, create bool) (string, string, bool, error) {
	googleProfile, err := client.CheckGoogleToken(ctx, idToken)
	if err != nil {
		logger.Error("Could not authenticate Google profile.", zap.Error(err))
		return "", "", false, status.Error(codes.Unauthenticated, "Could not authenticate Google profile.")
	}

	return authenticateProviderMySQL(ctx, logger, db, identityGoogleProfile(googleProfile), username, create)

}

func AuthenticateSteam(ctx context.Context, logger *zap.Logger, db *sql.DB, client *social.Client, appID int, publisherKey, token, username string, create bool) (string, string, string, bool, error) {
	steamProfile, err := client.GetSteamProfile(ctx, publisherKey, appID, token)
	if err != nil {
		logger.Info("Could not authenticate Steam profile.", zap.Error(err))
		return "", "", "", false, status.Error(codes.Unauthenticated, "Could not authenticate Steam profile.")
	}
	steamID := strconv.FormatUint(steamProfile.SteamID, 10)
	{
		id, name, created, err := authenticateProviderMySQL(ctx, logger, db, identityProviderProfile{column: "steam_id", id: steamID}, username, create)
		if err != nil {
			return "", "", "", false, err
		}
		return id, name, steamID, created, nil
	}

}

func importSteamFriends(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, messageRouter MessageRouter, client *social.Client, userID uuid.UUID, username, publisherKey, steamId string, reset bool) error {
	logger = logger.With(zap.String("userID", userID.String()))

	steamProfiles, err := client.GetSteamFriends(ctx, publisherKey, steamId)
	var unauthorizedErr *social.UnauthorizedError
	if err != nil && !errors.As(err, &unauthorizedErr) {
		// If error is unauthorized it means the profile or friends is private, ignore.
		logger.Error("Could not import Steam friends.", zap.Error(err))
		return status.Error(codes.Unauthenticated, "Could not authenticate Steam profile.")
	}

	if len(steamProfiles) == 0 && !reset {
		// No Steam friends to import, and friend reset not requested - no work to do.
		return nil
	}

	providerIDs := make([]string, 0, len(steamProfiles))
	for _, profile := range steamProfiles {
		providerIDs = append(providerIDs, strconv.FormatUint(profile.SteamID, 10))
	}

	return importProviderFriendsMySQL(ctx, logger, db, tracker, messageRouter, userID, username, "steam_id", providerIDs, "Steam", reset)

}

func importFacebookFriends(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, messageRouter MessageRouter, client *social.Client, userID uuid.UUID, username, token string, reset bool) error {
	logger = logger.With(zap.String("userID", userID.String()))

	facebookProfiles, err := client.GetFacebookFriends(ctx, token)
	if err != nil {
		logger.Error("Could not import Facebook friends.", zap.Error(err))
		return status.Error(codes.Unauthenticated, "Could not authenticate Facebook profile.")
	}

	if len(facebookProfiles) == 0 && !reset {
		// No Facebook friends to import, and friend reset not requested - no work to do.
		return nil
	}

	providerIDs := make([]string, 0, len(facebookProfiles))
	for _, profile := range facebookProfiles {
		providerIDs = append(providerIDs, profile.ID)
	}

	return importProviderFriendsMySQL(ctx, logger, db, tracker, messageRouter, userID, username, "facebook_id", providerIDs, "Facebook", reset)

}

func sendFriendAddedNotification(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, messageRouter MessageRouter, userID uuid.UUID, username string, friendUserIDs []uuid.UUID) {
	notifications := make(map[uuid.UUID][]*api.Notification, len(friendUserIDs))
	content, _ := json.Marshal(map[string]interface{}{"username": username})
	subject := "Your friend has just joined the game"
	createTime := time.Now().UTC().Unix()
	for _, friendUserID := range friendUserIDs {
		notifications[friendUserID] = []*api.Notification{{
			Id:         uuid.Must(uuid.NewV4()).String(),
			Subject:    subject,
			Content:    string(content),
			SenderId:   userID.String(),
			Code:       NotificationCodeFriendJoinGame,
			Persistent: true,
			CreateTime: &timestamppb.Timestamp{Seconds: createTime},
		}}
	}
	// Any error is already logged before it's returned here.
	_ = NotificationSend(ctx, logger, db, tracker, messageRouter, notifications)
}
