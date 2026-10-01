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
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/internal/cronexpr"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Internal error used to signal out of transactional wrappers.
var errTournamentWriteNoop = errors.New("tournament write noop")

type TournamentListCursor struct {
	Id string
}

func TournamentCreate(ctx context.Context, logger *zap.Logger, cache LeaderboardCache, scheduler LeaderboardScheduler, leaderboardId string, authoritative bool, sortOrder, operator int, resetSchedule, metadata,
	title, description string, category, startTime, endTime, duration, maxSize, maxNumScore int, joinRequired, enableRanks bool) error {

	_, created, err := cache.CreateTournament(ctx, leaderboardId, authoritative, sortOrder, operator, resetSchedule, metadata, title, description, category, startTime, endTime, duration, maxSize, maxNumScore, joinRequired, enableRanks)
	if err != nil {
		return err
	}

	if created {
		// Only need to update the scheduler for newly created tournaments.
		scheduler.Update()
	}

	return nil
}

func TournamentDelete(ctx context.Context, cache LeaderboardCache, rankCache LeaderboardRankCache, scheduler LeaderboardScheduler, leaderboardId string) error {
	leaderboard := cache.Get(leaderboardId)
	if leaderboard == nil || !leaderboard.IsTournament() {
		// If it does not exist treat it as success.
		return nil
	}

	_, err := cache.Delete(ctx, rankCache, scheduler, leaderboardId)
	if err != nil {
		return err
	}

	return nil
}

func TournamentAddAttempt(ctx context.Context, logger *zap.Logger, db *sql.DB, cache LeaderboardCache, leaderboardId string, owner string, count int) error {

	return tournamentAddAttemptMySQL(ctx, db, leaderboardId, owner, count)

}

func TournamentJoin(ctx context.Context, logger *zap.Logger, db *sql.DB, cache LeaderboardCache, rankCache LeaderboardRankCache, ownerID uuid.UUID, username, tournamentId string) error {

	return tournamentJoinMySQL(ctx, db, rankCache, ownerID, username, tournamentId)

}

func TournamentsGet(ctx context.Context, logger *zap.Logger, db *sql.DB, leaderboardCache LeaderboardCache, tournamentIDs []string) ([]*api.Tournament, error) {
	now := time.Now().UTC()

	records := make([]*api.Tournament, 0, len(tournamentIDs))
	uniqueTournamentIDs := make(map[string]struct{}, len(tournamentIDs))
	dbLookupTournamentIDs := make([]string, 0, 1)
	for _, tournamentID := range tournamentIDs {
		if _, found := uniqueTournamentIDs[tournamentID]; found {
			continue
		}
		uniqueTournamentIDs[tournamentID] = struct{}{}

		tournament := leaderboardCache.Get(tournamentID)
		if tournament == nil || !tournament.IsTournament() {
			continue
		}
		if tournament.HasMaxSize() {
			dbLookupTournamentIDs = append(dbLookupTournamentIDs, tournamentID)
			continue
		}

		canEnter := true
		endTime := tournament.EndTime

		startActive, endActiveUnix, expiryUnix := calculateTournamentDeadlines(tournament.StartTime, endTime, int64(tournament.Duration), tournament.ResetSchedule, now)

		if startActive > now.Unix() || (endActiveUnix != 0 && endActiveUnix < now.Unix()) {
			canEnter = false
		}

		var prevReset int64
		if tournament.ResetSchedule != nil {
			prevReset = calculatePrevReset(now, tournament.StartTime, tournament.ResetSchedule)
		}

		tournamentRecord := &api.Tournament{
			Id:            tournament.Id,
			Title:         tournament.Title,
			Description:   tournament.Description,
			Category:      uint32(tournament.Category),
			SortOrder:     uint32(tournament.SortOrder),
			Operator:      OperatorIntToEnum[tournament.Operator],
			Size:          0,
			MaxSize:       uint32(tournament.MaxSize),
			MaxNumScore:   uint32(tournament.MaxNumScore),
			CanEnter:      canEnter,
			EndActive:     uint32(endActiveUnix),
			PrevReset:     uint32(prevReset),
			NextReset:     uint32(expiryUnix),
			Metadata:      tournament.Metadata,
			CreateTime:    &timestamppb.Timestamp{Seconds: tournament.CreateTime},
			StartTime:     &timestamppb.Timestamp{Seconds: tournament.StartTime},
			Duration:      uint32(tournament.Duration),
			StartActive:   uint32(startActive),
			Authoritative: tournament.Authoritative,
		}

		if endTime > 0 {
			tournamentRecord.EndTime = &timestamppb.Timestamp{Seconds: endTime}
		}

		records = append(records, tournamentRecord)
	}

	if len(dbLookupTournamentIDs) > 0 {
		query := ""

		// Retrieved directly from database to have the latest configuration and 'size' etc field values.
		// Ensures consistency between return data from this call and TournamentList.
		params := []any{dbLookupTournamentIDs}
		{
			query, params = tournamentQueryMySQL(tournamentDefinitionColumns, dbLookupTournamentIDs)
		}
		rows, err := db.QueryContext(ctx, query, params...)
		if err != nil {
			logger.Error("Could not retrieve tournaments", zap.Error(err))
			return nil, err
		}

		for rows.Next() {
			tournament, err := parseTournament(rows, now)
			if err != nil {
				if errors.Is(err, runtime.ErrTournamentNotFound) {
					// This ID mapped to a non-tournament leaderboard, just skip it.
					continue
				}

				_ = rows.Close()
				logger.Error("Error parsing retrieved tournament records", zap.Error(err))
				return nil, err
			}

			records = append(records, tournament)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	return records, nil
}

func TournamentList(ctx context.Context, logger *zap.Logger, db *sql.DB, leaderboardCache LeaderboardCache, categoryStart, categoryEnd, startTime, endTime, limit int, cursor *TournamentListCursor) (*api.TournamentList, error) {
	now := time.Now().UTC()
	nowUnix := now.Unix()

	list, newCursor, err := leaderboardCache.ListTournaments(nowUnix, categoryStart, categoryEnd, int64(startTime), int64(endTime), limit, cursor)
	if err != nil {
		logger.Error("Could not retrieve tournaments", zap.Error(err))
		return nil, err
	}

	if len(list) == 0 {
		return &api.TournamentList{
			Tournaments: []*api.Tournament{},
		}, nil
	}

	// Read most up to date sizes from database.
	ids := make([]string, 0, len(list))
	for _, leaderboard := range list {
		if !leaderboard.HasMaxSize() {
			continue
		}
		ids = append(ids, leaderboard.Id)
	}

	sizes := make(map[string]int, len(list))
	if len(ids) > 0 {
		query := ""
		params := []any{ids}
		{
			query, params = tournamentQueryMySQL("id,size", ids)
		}
		rows, err := db.QueryContext(ctx, query, params...)
		if err != nil {
			logger.Error("Could not retrieve tournaments", zap.Error(err))
			return nil, err
		}

		var dbID string
		var dbSize int
		for rows.Next() {
			if err := rows.Scan(&dbID, &dbSize); err != nil {
				_ = rows.Close()
				logger.Error("Error parsing listed tournament records", zap.Error(err))
				return nil, err
			}
			sizes[dbID] = dbSize
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	records := make([]*api.Tournament, 0, len(list))
	for _, leaderboard := range list {
		size := sizes[leaderboard.Id]
		startActive, endActiveUnix, expiryUnix := calculateTournamentDeadlines(leaderboard.StartTime, leaderboard.EndTime, int64(leaderboard.Duration), leaderboard.ResetSchedule, now)
		canEnter := true

		if startActive > nowUnix || endActiveUnix < nowUnix {
			canEnter = false
		}
		if canEnter && size >= leaderboard.MaxSize {
			canEnter = false
		}

		var prevReset int64
		if leaderboard.ResetSchedule != nil {
			prevReset = calculatePrevReset(now, leaderboard.StartTime, leaderboard.ResetSchedule)
		}

		record := &api.Tournament{
			Id:            leaderboard.Id,
			Title:         leaderboard.Title,
			Description:   leaderboard.Description,
			Category:      uint32(leaderboard.Category),
			SortOrder:     uint32(leaderboard.SortOrder),
			Operator:      OperatorIntToEnum[leaderboard.Operator],
			Size:          uint32(size),
			MaxSize:       uint32(leaderboard.MaxSize),
			MaxNumScore:   uint32(leaderboard.MaxNumScore),
			CanEnter:      canEnter,
			EndActive:     uint32(endActiveUnix),
			PrevReset:     uint32(prevReset),
			NextReset:     uint32(expiryUnix),
			Metadata:      leaderboard.Metadata,
			CreateTime:    &timestamppb.Timestamp{Seconds: leaderboard.CreateTime},
			StartTime:     &timestamppb.Timestamp{Seconds: leaderboard.StartTime},
			Duration:      uint32(leaderboard.Duration),
			StartActive:   uint32(startActive),
			Authoritative: leaderboard.Authoritative,
			JoinRequired:  leaderboard.JoinRequired,
		}
		if leaderboard.EndTime != 0 {
			record.EndTime = &timestamppb.Timestamp{Seconds: leaderboard.EndTime}
		}
		records = append(records, record)
	}

	tournamentList := &api.TournamentList{
		Tournaments: records,
	}
	if newCursor != nil {
		cursorBuf := new(bytes.Buffer)
		if err := gob.NewEncoder(cursorBuf).Encode(newCursor); err != nil {
			logger.Error("Error creating tournament records list cursor", zap.Error(err))
			return nil, err
		}
		tournamentList.Cursor = base64.URLEncoding.EncodeToString(cursorBuf.Bytes())
	}

	return tournamentList, nil
}

func TournamentRecordsList(ctx context.Context, logger *zap.Logger, db *sql.DB, leaderboardCache LeaderboardCache, rankCache LeaderboardRankCache, tournamentId string, ownerIds []string, limit *wrapperspb.Int32Value, cursor string, overrideExpiry int64) (*api.TournamentRecordList, error) {
	leaderboard := leaderboardCache.Get(tournamentId)
	if leaderboard == nil || !leaderboard.IsTournament() {
		return nil, runtime.ErrTournamentNotFound
	}

	if overrideExpiry == 0 && leaderboard.EndTime > 0 && leaderboard.EndTime <= time.Now().UTC().Unix() {
		return nil, runtime.ErrTournamentOutsideDuration
	}

	records, err := LeaderboardRecordsList(ctx, logger, db, leaderboardCache, rankCache, tournamentId, limit, cursor, ownerIds, overrideExpiry)
	if err != nil {
		logger.Error("Error listing records from tournament.", zap.Error(err))
		return nil, err
	}

	recordList := &api.TournamentRecordList{
		Records:      records.Records,
		OwnerRecords: records.OwnerRecords,
		NextCursor:   records.NextCursor,
		PrevCursor:   records.PrevCursor,
		RankCount:    records.RankCount,
	}

	return recordList, nil
}

func TournamentRecordWrite(ctx context.Context, logger *zap.Logger, db *sql.DB, leaderboardCache LeaderboardCache, rankCache LeaderboardRankCache, caller uuid.UUID, tournamentId string, ownerId uuid.UUID, username string, score, subscore int64, metadata string, overrideOperator api.Operator) (*api.LeaderboardRecord, error) {

	return leaderboardRecordWriteMySQL(ctx, db, rankCache, caller, tournamentId, ownerId, username, score, subscore, metadata, overrideOperator, true)

}

func TournamentRecordDelete(ctx context.Context, logger *zap.Logger, db *sql.DB, leaderboardCache LeaderboardCache, rankCache LeaderboardRankCache, caller uuid.UUID, tournamentID, ownerID string) error {

	return leaderboardRecordDeleteMySQL(ctx, db, rankCache, caller, tournamentID, ownerID, true)

}

func TournamentRecordsHaystack(ctx context.Context, logger *zap.Logger, db *sql.DB, leaderboardCache LeaderboardCache, rankCache LeaderboardRankCache, leaderboardId, cursor string, ownerId uuid.UUID, limit int, expiryOverride int64) (*api.TournamentRecordList, error) {
	leaderboard := leaderboardCache.Get(leaderboardId)
	if leaderboard == nil || !leaderboard.IsTournament() {
		return nil, ErrLeaderboardNotFound
	}

	sortOrder := leaderboard.SortOrder

	expiry := expiryOverride
	if expiry == 0 {
		now := time.Now().UTC()
		_, _, expiry = calculateTournamentDeadlines(leaderboard.StartTime, leaderboard.EndTime, int64(leaderboard.Duration), leaderboard.ResetSchedule, now)
		if expiry != 0 && expiry <= now.Unix() {
			// if the expiry time is in the past, we wont have any records to return
			return &api.TournamentRecordList{Records: []*api.LeaderboardRecord{}}, nil
		}
	}

	expiryTime := time.Unix(expiry, 0).UTC()

	results, err := getLeaderboardRecordsHaystack(ctx, logger, db, leaderboardCache, rankCache, ownerId, limit, leaderboard.Id, cursor, sortOrder, expiryTime)
	if err != nil {
		return nil, err
	}

	tournamentRecordList := &api.TournamentRecordList{Records: results.Records, NextCursor: results.NextCursor, PrevCursor: results.PrevCursor, RankCount: results.RankCount}

	return tournamentRecordList, nil
}

func calculateTournamentDeadlines(startTime, endTime, duration int64, resetSchedule *cronexpr.Expression, t time.Time) (int64, int64, int64) {
	tUnix := t.UTC().Unix()
	if resetSchedule != nil {
		var startActiveUnix int64

		if tUnix < startTime {
			//  the supplied time is behind the start time
			startActiveUnix = resetSchedule.Next(time.Unix(startTime, 0).UTC()).UTC().Unix()
		} else {
			// check if we are landing squarely on the reset schedule
			landsOnSched := resetSchedule.Next(t.Add(-1*time.Second)).Unix() == t.Unix()
			if landsOnSched {
				startActiveUnix = tUnix
			} else {
				startActiveUnix = resetSchedule.Last(t).UTC().Unix()
			}
		}

		// endActiveUnix is when the current iteration ends.
		endActiveUnix := startActiveUnix + duration
		// expiryUnix represent the start of the next schedule, i.e., when the next iteration begins. It's when the current records "expire".
		expiryUnix := resetSchedule.Next(time.Unix(startActiveUnix, 0).UTC()).UTC().Unix()

		if endActiveUnix > expiryUnix {
			// Cap the end active to the same time as the expiry.
			endActiveUnix = expiryUnix
		}

		if startTime > endActiveUnix {
			// The start time after the end of the current active period but before the next reset.
			// e.g. Reset schedule is daily at noon, duration is 1 hour, but time is currently 3pm.
			schedules := resetSchedule.NextN(time.Unix(startTime, 0).UTC(), 2)
			startActiveUnix = schedules[0].UTC().Unix()
			endActiveUnix = startActiveUnix + duration
			expiryUnix = schedules[1].UTC().Unix()
			if endActiveUnix > expiryUnix {
				// Cap the end active to the same time as the expiry.
				endActiveUnix = expiryUnix
			}
		} else if startTime > startActiveUnix {
			startActiveUnix = startTime
		}

		if endTime > 0 && expiryUnix > endTime {
			expiryUnix = endTime
			if endActiveUnix > expiryUnix {
				// Cap the end active to the same time as the expiry.
				endActiveUnix = expiryUnix
			}
		}

		return startActiveUnix, endActiveUnix, expiryUnix
	}

	endActiveUnix := startTime + duration
	expiryUnix := endTime
	if endTime > 0 && endActiveUnix > endTime {
		// Cap the end active to the same time as the expiry.
		endActiveUnix = endTime
	}
	return startTime, endActiveUnix, expiryUnix
}

func parseTournament(scannable Scannable, now time.Time) (*api.Tournament, error) {
	var dbID string
	var dbSortOrder int
	var dbOperator int
	var dbResetSchedule sql.NullString
	var dbMetadata string
	var dbCreateTime sql.NullTime
	var dbCategory int
	var dbDescription string
	var dbDuration int
	var dbEndTime sql.NullTime
	var dbMaxSize int
	var dbMaxNumScore int
	var dbTitle string
	var dbSize int
	var dbStartTime sql.NullTime
	err := scannable.Scan(&dbID, &dbSortOrder, &dbOperator, &dbResetSchedule, &dbMetadata, &dbCreateTime,
		&dbCategory, &dbDescription, &dbDuration, &dbEndTime, &dbMaxSize, &dbMaxNumScore, &dbTitle, &dbSize, &dbStartTime)
	if err != nil {
		return nil, err
	}
	if dbDuration <= 0 {
		return nil, runtime.ErrTournamentNotFound
	}

	var resetSchedule *cronexpr.Expression
	if dbResetSchedule.Valid {
		resetSchedule = cronexpr.MustParse(dbResetSchedule.String)
	}

	canEnter := true
	endTime := dbEndTime.Time.UTC().Unix()

	startActiveUnix, endActiveUnix, expiryUnix := calculateTournamentDeadlines(dbStartTime.Time.UTC().Unix(), endTime, int64(dbDuration), resetSchedule, now)

	if startActiveUnix > now.Unix() || (endActiveUnix != 0 && endActiveUnix < now.Unix()) {
		canEnter = false
	}

	if canEnter && dbSize >= dbMaxSize {
		canEnter = false
	}

	var prevReset int64
	if resetSchedule != nil {
		prevReset = calculatePrevReset(now, dbStartTime.Time.UTC().Unix(), resetSchedule)
	}

	tournament := &api.Tournament{
		Id:          dbID,
		Title:       dbTitle,
		Description: dbDescription,
		Category:    uint32(dbCategory),
		SortOrder:   uint32(dbSortOrder),
		Operator:    OperatorIntToEnum[dbOperator],
		Size:        uint32(dbSize),
		MaxSize:     uint32(dbMaxSize),
		MaxNumScore: uint32(dbMaxNumScore),
		CanEnter:    canEnter,
		StartActive: uint32(startActiveUnix),
		EndActive:   uint32(endActiveUnix),
		PrevReset:   uint32(prevReset),
		NextReset:   uint32(expiryUnix),
		Metadata:    dbMetadata,
		CreateTime:  &timestamppb.Timestamp{Seconds: dbCreateTime.Time.UTC().Unix()},
		StartTime:   &timestamppb.Timestamp{Seconds: dbStartTime.Time.UTC().Unix()},
		Duration:    uint32(dbDuration),
	}

	if endTime > 0 {
		tournament.EndTime = &timestamppb.Timestamp{Seconds: endTime}
	}

	return tournament, nil
}

func DisableTournamentRanks(ctx context.Context, logger *zap.Logger, db *sql.DB, leaderboardCache LeaderboardCache, rankCache LeaderboardRankCache, id string) error {
	l := leaderboardCache.Get(id)
	if l == nil || !l.IsTournament() {
		return runtime.ErrTournamentNotFound
	}

	query := "UPDATE leaderboard SET enable_ranks = false WHERE id = ?"
	if _, err := db.ExecContext(ctx, query, id); err != nil {
		logger.Error("failed to set leaderboard enable_ranks value", zap.Error(err))
		return errors.New("failed to disable leaderboard ranks")
	}

	leaderboardCache.InsertTournament(l.Id, l.Authoritative, l.SortOrder, l.Operator, l.ResetScheduleStr, l.Metadata, l.Title, l.Description, l.Category, l.Duration, l.MaxSize, l.MaxNumScore, l.JoinRequired, l.CreateTime, l.StartTime, l.EndTime, false)

	_, _, expiryUnix := calculateTournamentDeadlines(l.StartTime, l.EndTime, int64(l.Duration), l.ResetSchedule, time.Now().UTC())
	rankCache.DeleteLeaderboard(l.Id, expiryUnix)

	return nil
}
