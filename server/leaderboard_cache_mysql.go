// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/heroiclabs/nakama/v3/internal/cronexpr"
)

const leaderboardDefinitionColumns = "id,authoritative,sort_order,operator,COALESCE(reset_schedule,''),metadata,create_time,category,description,duration,end_time,join_required,max_size,max_num_score,title,start_time,enable_ranks"
const tournamentDefinitionColumns = "id,sort_order,operator,reset_schedule,metadata,create_time,category,description,duration,end_time,max_size,max_num_score,title,size,start_time"

func scanLeaderboardDefinition(row Scannable) (*Leaderboard, error) {
	l := &Leaderboard{}
	var created, start, end time.Time
	if err := row.Scan(&l.Id, &l.Authoritative, &l.SortOrder, &l.Operator, &l.ResetScheduleStr, &l.Metadata, &created, &l.Category, &l.Description, &l.Duration, &end, &l.JoinRequired, &l.MaxSize, &l.MaxNumScore, &l.Title, &start, &l.EnableRanks); err != nil {
		return nil, err
	}
	l.CreateTime, l.StartTime, l.EndTime = created.Unix(), start.Unix(), end.Unix()
	if l.ResetScheduleStr != "" {
		var err error
		l.ResetSchedule, err = cronexpr.Parse(l.ResetScheduleStr)
		if err != nil {
			return nil, err
		}
	}
	return l, nil
}

// Read back the winner's entire definition, including enable_ranks, on duplicate
// creation. No cache entry is published before the database commit succeeds.
func (l *LocalLeaderboardCache) createMySQL(ctx context.Context, requested *Leaderboard) (*Leaderboard, bool, error) {
	l.Lock()
	defer l.Unlock()
	if existing := l.leaderboards[requested.Id]; existing != nil {
		if requested.IsTournament() && !existing.IsTournament() {
			return nil, false, errors.New("cannot create tournament as leaderboard is already in use")
		}
		return existing, false, nil
	}
	var stored *Leaderboard
	var created bool
	err := executeInTxMySQL(ctx, l.db, func(tx *sql.Tx) error {
		created = false
		var reset any
		if requested.ResetScheduleStr != "" {
			reset = requested.ResetScheduleStr
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO leaderboard (id,authoritative,sort_order,operator,reset_schedule,metadata,category,description,duration,end_time,join_required,max_size,max_num_score,title,start_time,enable_ranks) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			requested.Id, requested.Authoritative, requested.SortOrder, requested.Operator, reset, requested.Metadata, requested.Category, requested.Description, requested.Duration, time.Unix(requested.EndTime, 0).UTC(), requested.JoinRequired, requested.MaxSize, requested.MaxNumScore, requested.Title, time.Unix(requested.StartTime, 0).UTC(), requested.EnableRanks)
		if err != nil && !mysqlErrorNumber(err, 1062) {
			return err
		}
		created = err == nil
		stored, err = scanLeaderboardDefinition(tx.QueryRowContext(ctx, "SELECT "+leaderboardDefinitionColumns+" FROM leaderboard WHERE id = ? FOR UPDATE", requested.Id))
		if err != nil {
			return err
		}
		if requested.IsTournament() && !stored.IsTournament() {
			return errors.New("cannot create tournament as leaderboard is already in use")
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	l.leaderboards[stored.Id] = stored
	l.allList = append(l.allList, stored)
	if stored.IsTournament() {
		l.tournamentList = append(l.tournamentList, stored)
		sort.Sort(OrderedTournaments(l.tournamentList))
	} else {
		l.leaderboardList = append(l.leaderboardList, stored)
	}
	return stored, created, nil
}
