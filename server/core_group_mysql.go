// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const mysqlGroupSelect = "SELECT id, creator_id, name, description, avatar_url, state, edge_count, lang_tag, max_count, metadata, create_time, update_time FROM `groups`"
const mysqlActiveGroup = "disable_time = '1970-01-01 00:00:00'"

type mysqlLockedGroup struct {
	name              string
	state, count, max int
	disabled          time.Time
}

// Lock users before the group, matching account deletion. All membership writers
// take the group lock, so permission, capacity and last-superadmin decisions see
// the preceding committed mutation. No network or in-memory effects run in a retry.
func lockGroupUsersMySQL(ctx context.Context, tx *sql.Tx, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	unique := map[string]uuid.UUID{}
	for _, id := range ids {
		if id != uuid.Nil {
			unique[id.String()] = id
		}
	}
	ordered := make([]string, 0, len(unique))
	for id := range unique {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	users := make(map[uuid.UUID]string, len(ordered))
	for _, id := range ordered {
		var name string
		err := tx.QueryRowContext(ctx, "SELECT username FROM users WHERE id = ? FOR UPDATE", id).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		users[unique[id]] = name
	}
	return users, nil
}

func lockGroupMySQL(ctx context.Context, tx *sql.Tx, id uuid.UUID) (*mysqlLockedGroup, error) {
	g := &mysqlLockedGroup{}
	err := tx.QueryRowContext(ctx, "SELECT name,state,edge_count,max_count,disable_time FROM `groups` WHERE id = ? FOR UPDATE", id).Scan(&g.name, &g.state, &g.count, &g.max, &g.disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return g, err
}
func groupStateMySQL(ctx context.Context, tx *sql.Tx, gid, uid uuid.UUID) (int, error) {
	var state int
	err := tx.QueryRowContext(ctx, "SELECT state FROM group_edge WHERE source_id = ? AND destination_id = ?", gid, uid).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	return state, err
}
func groupPermissionMySQL(ctx context.Context, tx *sql.Tx, gid, caller uuid.UUID, max int) (int, error) {
	if caller == uuid.Nil {
		return 0, nil
	}
	state, err := groupStateMySQL(ctx, tx, gid, caller)
	if err != nil {
		return 0, err
	}
	if state < 0 || state > max {
		return 0, runtime.ErrGroupPermissionDenied
	}
	return state, nil
}
func deleteGroupMySQL(ctx context.Context, tx *sql.Tx, gid uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM `groups` WHERE id = ?", gid); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM group_edge WHERE source_id = ? OR destination_id = ?", gid, gid)
	return err
}
func removeGroupPairMySQL(ctx context.Context, tx *sql.Tx, gid, uid uuid.UUID) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM group_edge WHERE (source_id = ? AND destination_id = ?) OR (source_id = ? AND destination_id = ?)", gid, uid, uid, gid)
	return err
}

// Choose a position unique across every state for both owners. Promotions can
// then change state without colliding with another edge's primary key.
func setGroupPairMySQL(ctx context.Context, tx *sql.Tx, gid, uid uuid.UUID, state int) error {
	var last int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(`position`),0) FROM group_edge WHERE source_id IN (?,?)", gid, uid).Scan(&last); err != nil {
		return err
	}
	position := time.Now().UTC().UnixNano()
	if last >= position {
		if last == math.MaxInt64 {
			return errors.New("group position exhausted")
		}
		position = last + 1
	}
	if err := removeGroupPairMySQL(ctx, tx, gid, uid); err != nil {
		return err
	}
	query := "INSERT INTO group_edge (`position`,state,source_id,destination_id) VALUES (?,?,?,?)"
	args := []any{position, state, gid, uid}
	// Bans are deliberately one-way, as in the original group API.
	if state != BANNED_CODE {
		query += ",(?,?,?,?)"
		args = append(args, position, state, uid, gid)
	}
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}
func changeGroupCountMySQL(ctx context.Context, tx *sql.Tx, gid uuid.UUID, g *mysqlLockedGroup, delta int) error {
	if delta > 0 && g.count+delta > g.max {
		return runtime.ErrGroupFull
	}
	if !g.disabled.Equal(time.Unix(0, 0)) {
		return runtime.ErrGroupNotFound
	}
	_, err := tx.ExecContext(ctx, "UPDATE `groups` SET edge_count = edge_count + ?, update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", delta, gid)
	if err == nil {
		g.count += delta
	}
	return err
}
func otherGroupSuperadminsMySQL(ctx context.Context, tx *sql.Tx, gid, uid uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM group_edge WHERE source_id = ? AND destination_id <> ? AND state = 0", gid, uid).Scan(&n)
	return n, err
}

func createGroupMySQL(ctx context.Context, db *sql.DB, uid, creator uuid.UUID, name, lang, desc, avatar, metadata string, open bool, max int) (*api.Group, error) {
	if uid == uuid.Nil {
		return nil, runtime.ErrGroupCreatorInvalid
	}
	if lang == "" {
		lang = "en"
	}
	if metadata == "" {
		metadata = "{}"
	}
	if max <= 0 {
		max = 100
	}
	state := 1
	if open {
		state = 0
	}
	gid := uuid.Must(uuid.NewV4())
	var result *api.Group
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		users, err := lockGroupUsersMySQL(ctx, tx, []uuid.UUID{uid})
		if err != nil {
			return err
		}
		if _, ok := users[uid]; !ok {
			return runtime.ErrGroupCreatorInvalid
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO `groups` (id,creator_id,name,lang_tag,description,avatar_url,metadata,state,max_count,edge_count) VALUES (?,?,?,?,?,?,?,?,?,1)", gid, creator, name, lang, desc, avatar, metadata, state, max)
		if mysqlErrorNumber(err, 1062) {
			return runtime.ErrGroupNameInUse
		}
		if err != nil {
			return err
		}
		if err = setGroupPairMySQL(ctx, tx, gid, uid, 0); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, mysqlGroupSelect+" WHERE id = ?", gid)
		if err != nil {
			return err
		}
		groups, _, err := groupConvertRows(rows, 1)
		if err != nil {
			return err
		}
		result = groups[0]
		return nil
	})
	return result, err
}

func updateGroupMySQL(ctx context.Context, db *sql.DB, gid, uid, creator uuid.UUID, name, lang, desc, avatar, metadata *wrapperspb.StringValue, open *wrapperspb.BoolValue, max int) error {
	return executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		if _, err := lockGroupUsersMySQL(ctx, tx, []uuid.UUID{uid}); err != nil {
			return err
		}
		g, err := lockGroupMySQL(ctx, tx, gid)
		if err != nil {
			return err
		}
		if _, err = groupPermissionMySQL(ctx, tx, gid, uid, 1); err != nil {
			return err
		}
		fields, args := []string{}, []any{}
		for _, f := range []struct {
			column   string
			value    *wrapperspb.StringValue
			nullable bool
		}{{"name", name, false}, {"lang_tag", lang, false}, {"description", desc, true}, {"avatar_url", avatar, true}, {"metadata", metadata, false}} {
			if f.value == nil {
				continue
			}
			fields = append(fields, f.column+" = ?")
			if f.nullable && f.value.Value == "" {
				args = append(args, nil)
			} else {
				args = append(args, f.value.Value)
			}
		}
		if open != nil {
			state := 1
			if open.Value {
				state = 0
			}
			fields = append(fields, "state = ?")
			args = append(args, state)
		}
		if max > 0 {
			fields = append(fields, "max_count = ?")
			args = append(args, max)
		}
		if creator != uuid.Nil {
			fields = append(fields, "creator_id = ?")
			args = append(args, creator)
		}
		if len(fields) == 0 {
			return runtime.ErrGroupNoUpdateOps
		}
		if g == nil || !g.disabled.Equal(time.Unix(0, 0)) {
			return runtime.ErrGroupNotUpdated
		}
		args = append(args, gid)
		_, err = tx.ExecContext(ctx, "UPDATE `groups` SET update_time = CURRENT_TIMESTAMP(6), "+strings.Join(fields, ",")+" WHERE id = ?", args...)
		if mysqlErrorNumber(err, 1062) {
			return runtime.ErrGroupNameInUse
		}
		return err
	})
}

func deleteGroupWithPermissionMySQL(ctx context.Context, db *sql.DB, tracker Tracker, gid, caller uuid.UUID) error {
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		if _, err := lockGroupUsersMySQL(ctx, tx, []uuid.UUID{caller}); err != nil {
			return err
		}
		if _, err := lockGroupMySQL(ctx, tx, gid); err != nil {
			return err
		}
		if _, err := groupPermissionMySQL(ctx, tx, gid, caller, 0); err != nil {
			return err
		}
		return deleteGroupMySQL(ctx, tx, gid)
	})
	if err == nil {
		tracker.UntrackByStream(PresenceStream{Mode: StreamModeGroup, Subject: gid})
	}
	return err
}

type mysqlGroupEffects struct {
	messages      []*api.ChannelMessage
	notifications map[uuid.UUID][]*api.Notification
	evict         map[uuid.UUID]bool
	deleted       bool
}

func (e *mysqlGroupEffects) notify(uid, sender uuid.UUID, code int32, subject string, content map[string]string) {
	payload, _ := json.Marshal(content)
	e.notifications[uid] = []*api.Notification{{Id: uuid.Must(uuid.NewV4()).String(), SenderId: sender.String(), Code: code, Subject: subject, Content: string(payload), Persistent: true, CreateTime: timestamppb.New(time.Now().UTC())}}
}
func (e *mysqlGroupEffects) message(ctx context.Context, tx *sql.Tx, gid, uid uuid.UUID, username string, code int32) error {
	stream := PresenceStream{Mode: StreamModeGroup, Subject: gid}
	channelID, err := StreamToChannelId(stream)
	if err != nil {
		return err
	}
	ts := time.Now().UTC().Truncate(time.Second)
	m := &api.ChannelMessage{ChannelId: channelID, MessageId: uuid.Must(uuid.NewV4()).String(), Code: wrapperspb.Int32(code), SenderId: uid.String(), Username: username, Content: "{}", CreateTime: timestamppb.New(ts), UpdateTime: timestamppb.New(ts), Persistent: wrapperspb.Bool(true), GroupId: gid.String()}
	_, err = tx.ExecContext(ctx, "INSERT INTO message (id,code,sender_id,username,stream_mode,stream_subject,stream_descriptor,stream_label,content,create_time,update_time) VALUES (?,?,?,?,?,?,?,?,?,?,?)", m.MessageId, code, uid, username, stream.Mode, gid, uuid.Nil, "", "{}", ts, ts)
	if err == nil {
		e.messages = append(e.messages, m)
	}
	return err
}

// Public entry points select the action; this is a native membership transaction,
// not a SQL translator. Each batch is atomic, including its persisted channel events.
func mutateGroupMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, tracker Tracker, router MessageRouter, manager StreamManager, caller, gid uuid.UUID, targets []uuid.UUID, username, action string, strict bool) error {
	var effects mysqlGroupEffects
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		effects = mysqlGroupEffects{notifications: map[uuid.UUID][]*api.Notification{}, evict: map[uuid.UUID]bool{}}
		ids := append([]uuid.UUID{caller}, targets...)
		users, err := lockGroupUsersMySQL(ctx, tx, ids)
		if err != nil {
			return err
		}
		g, err := lockGroupMySQL(ctx, tx, gid)
		if err != nil {
			return err
		}
		myState := 0
		if action != "join" && action != "leave" {
			myState, err = groupPermissionMySQL(ctx, tx, gid, caller, 1)
			if err != nil {
				return err
			}
		}
		if g == nil || !g.disabled.Equal(time.Unix(0, 0)) {
			if action == "ban" || (action == "leave" && g == nil) {
				return nil
			}
			return runtime.ErrGroupNotFound
		}
		seen := map[uuid.UUID]bool{}
		for _, uid := range targets {
			if seen[uid] || (uid == caller && action != "join" && action != "leave") {
				continue
			}
			seen[uid] = true
			targetName, exists := users[uid]
			old, err := groupStateMySQL(ctx, tx, gid, uid)
			if err != nil {
				return err
			}
			if action == "join" || action == "leave" {
				targetName = username
			}
			code := int32(0)
			switch action {
			case "join":
				if !exists {
					return runtime.ErrGroupUserNotFound
				}
				if g.count >= g.max {
					return runtime.ErrGroupFull
				}
				if old >= 0 {
					continue
				}
				state := 2
				if g.state == 1 {
					state = 3
				}
				if err = setGroupPairMySQL(ctx, tx, gid, uid, state); err != nil {
					return err
				}
				if state == 3 {
					rows, err := tx.QueryContext(ctx, "SELECT destination_id FROM group_edge WHERE source_id = ? AND state IN (0,1)", gid)
					if err != nil {
						return err
					}
					for rows.Next() {
						var admin string
						if err = rows.Scan(&admin); err != nil {
							rows.Close()
							return err
						}
						effects.notify(uuid.Must(uuid.FromString(admin)), uid, NotificationCodeGroupJoinRequest, fmt.Sprintf("User %v wants to join your group", username), map[string]string{"group_id": gid.String(), "username": username})
					}
					rows.Close()
					if err = rows.Err(); err != nil {
						return err
					}
					continue
				}
				if err = changeGroupCountMySQL(ctx, tx, gid, g, 1); err != nil {
					return err
				}
				code = ChannelMessageTypeGroupJoin
			case "leave":
				if old < 0 || old == BANNED_CODE {
					continue
				}
				if old == 0 {
					n, err := otherGroupSuperadminsMySQL(ctx, tx, gid, uid)
					if err != nil {
						return err
					}
					if n == 0 {
						if g.count > 1 {
							return runtime.ErrGroupLastSuperadmin
						}
						if err = deleteGroupMySQL(ctx, tx, gid); err != nil {
							return err
						}
						effects.deleted = true
						return nil
					}
				}
				if err = removeGroupPairMySQL(ctx, tx, gid, uid); err != nil {
					return err
				}
				if old < 3 {
					if err = changeGroupCountMySQL(ctx, tx, gid, g, -1); err != nil {
						return err
					}
				}
				code = ChannelMessageTypeGroupLeave
				effects.evict[uid] = true
			case "add":
				if !exists {
					return runtime.ErrGroupUserNotFound
				}
				if old >= 0 && old != 3 {
					continue
				}
				if err = changeGroupCountMySQL(ctx, tx, gid, g, 1); err != nil {
					return err
				}
				if old == 3 {
					_, err = tx.ExecContext(ctx, "UPDATE group_edge SET state = 2, update_time = CURRENT_TIMESTAMP(6) WHERE (source_id = ? AND destination_id = ?) OR (source_id = ? AND destination_id = ?)", gid, uid, uid, gid)
				} else {
					err = setGroupPairMySQL(ctx, tx, gid, uid, 2)
				}
				if err != nil {
					return err
				}
				code = ChannelMessageTypeGroupAdd
				effects.notify(uid, caller, NotificationCodeGroupAdd, fmt.Sprintf("You've been added to group %v", g.name), map[string]string{"group_id": gid.String(), "name": g.name})
			case "kick", "ban":
				allowed := old >= 0 && (myState == 0 || old > 1)
				if allowed && old == 0 {
					n, err := otherGroupSuperadminsMySQL(ctx, tx, gid, uid)
					if err != nil {
						return err
					}
					allowed = n > 0
				}
				if !allowed {
					if strict {
						return sql.ErrNoRows
					}
					continue
				}
				if action == "ban" {
					err = setGroupPairMySQL(ctx, tx, gid, uid, BANNED_CODE)
				} else {
					err = removeGroupPairMySQL(ctx, tx, gid, uid)
				}
				if err != nil {
					return err
				}
				if old >= 3 {
					continue
				}
				if err = changeGroupCountMySQL(ctx, tx, gid, g, -1); err != nil {
					return err
				}
				code = ChannelMessageTypeGroupKick
				if action == "ban" {
					code = ChannelMessageTypeGroupBan
				}
				effects.evict[uid] = true
			case "promote", "demote", "console_promote", "console_demote":
				next := old - 1
				if action == "promote" || action == "console_promote" {
					maxState := 2
					if action == "console_promote" {
						maxState = 3
					}
					if old <= myState || old <= 0 || old > maxState {
						continue
					}
				} else {
					if old < myState || old < 0 || old >= 2 {
						continue
					}
					if myState == 0 {
						n, err := otherGroupSuperadminsMySQL(ctx, tx, gid, uid)
						if err != nil {
							return err
						}
						if n == 0 {
							continue
						}
					}
					next = old + 1
				}
				if !exists {
					return runtime.ErrGroupUserNotFound
				}
				if old == 3 && next == 2 {
					if err := changeGroupCountMySQL(ctx, tx, gid, g, 1); err != nil {
						return err
					}
				}
				// Existing pairs retain their ordering across role changes.
				_, err = tx.ExecContext(ctx, "UPDATE group_edge SET state = ?, update_time = CURRENT_TIMESTAMP(6) WHERE (source_id = ? AND destination_id = ?) OR (source_id = ? AND destination_id = ?)", next, gid, uid, uid, gid)
				if err != nil {
					return err
				}
				code = ChannelMessageTypeGroupPromote
				if action == "demote" || action == "console_demote" {
					code = ChannelMessageTypeGroupDemote
				}
			default:
				return errors.New("unknown group mutation")
			}
			if err = effects.message(ctx, tx, gid, uid, targetName, code); err != nil {
				return err
			}
		}
		if len(effects.messages) == 0 {
			if action == "console_promote" {
				return ErrEmptyMemberPromote
			}
			if action == "console_demote" {
				return ErrEmptyMemberDemote
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	stream := PresenceStream{Mode: StreamModeGroup, Subject: gid}
	if effects.deleted {
		tracker.UntrackByStream(stream)
		return nil
	}
	for _, message := range effects.messages {
		router.SendToStream(logger, stream, &rtapi.Envelope{Message: &rtapi.Envelope_ChannelMessage{ChannelMessage: message}}, true)
	}
	if len(effects.notifications) > 0 {
		_ = NotificationSend(ctx, logger, db, tracker, router, effects.notifications)
	}
	if len(effects.evict) > 0 {
		for _, presence := range tracker.ListByStream(stream, true, true) {
			if effects.evict[presence.UserID] {
				if err := manager.UserLeave(stream, presence.UserID, presence.ID.SessionID); err != nil {
					logger.Warn("Could not remove presence from the group channel stream.")
				}
			}
		}
	}
	return nil
}

// Native queries share the established API result decoders and cursor format.
func groupEdgesQueryMySQL(users bool, owner uuid.UUID, limit int, state *wrapperspb.Int32Value, cursor *edgeListCursor) (string, []any) {
	query := "SELECT g.id,g.creator_id,g.name,g.description,g.avatar_url,g.lang_tag,g.metadata,g.state,g.edge_count,g.max_count,g.create_time,g.update_time,ge.state,ge.`position` FROM `groups` g JOIN group_edge ge ON g.id = ge.destination_id WHERE ge.source_id = ?"
	if users {
		query = "SELECT u.id,u.username,u.display_name,u.avatar_url,u.lang_tag,u.location,u.timezone,u.metadata,u.apple_id,u.facebook_id,u.facebook_instant_game_id,u.google_id,u.gamecenter_id,u.steam_id,u.edge_count,u.create_time,u.update_time,ge.state,ge.`position` FROM users u JOIN group_edge ge ON u.id = ge.destination_id WHERE ge.source_id = ?"
	}
	args := []any{owner}
	if state != nil {
		query += " AND ge.state = ?"
		args = append(args, state.Value)
	} else {
		query += " AND ge.state BETWEEN 0 AND 3"
	}
	if cursor != nil {
		query += " AND (ge.state,ge.`position`) >= (?,?)"
		args = append(args, cursor.State, cursor.Position)
	}
	query += " ORDER BY ge.state,ge.`position`"
	if limit != 0 {
		query += " LIMIT ?"
		args = append(args, limit+1)
	}
	return query, args
}

func listGroupsQueryMySQL(name, lang string, open *bool, count, limit int, cursor *groupListCursor) (string, []any) {
	query := mysqlGroupSelect + " WHERE " + mysqlActiveGroup
	args := []any{}
	if name != "" {
		query += " AND LOWER(name) LIKE LOWER(?)"
		args = append(args, name)
	}
	if lang != "" {
		query += " AND lang_tag = ?"
		args = append(args, lang)
	}
	if open != nil {
		state := 1
		if *open {
			state = 0
		}
		query += " AND state = ?"
		args = append(args, state)
	}
	if count >= 0 {
		query += " AND edge_count <= ?"
		args = append(args, count)
	}
	columns := []string{"update_time", "edge_count", "id"}
	desc := false
	switch {
	case name != "":
		columns = []string{"name"}
	case open != nil && lang != "":
		columns = []string{"edge_count", "id"}
		desc = true
	case open != nil && count >= 0:
		columns = []string{"edge_count", "lang_tag", "id"}
		desc = true
	case lang != "":
		columns = []string{"edge_count", "id"}
		desc = count >= 0
	case count >= 0:
		columns = []string{"edge_count", "update_time", "id"}
		desc = true
	}
	if cursor != nil {
		cmp := ">"
		if desc {
			cmp = "<"
		}
		query += " AND (" + strings.Join(columns, ",") + ") " + cmp + " (" + mysqlMarks(len(columns)) + ")"
		for _, col := range columns {
			switch col {
			case "name":
				args = append(args, cursor.Name)
			case "edge_count":
				args = append(args, cursor.EdgeCount)
			case "id":
				args = append(args, cursor.ID)
			case "lang_tag":
				args = append(args, cursor.Lang)
			case "update_time":
				args = append(args, cursor.GetUpdateTime())
			}
		}
	}
	order := append([]string(nil), columns...)
	if desc {
		for i := range order {
			order[i] += " DESC"
		}
	}
	query += " ORDER BY " + strings.Join(order, ",") + " LIMIT ?"
	args = append(args, limit+1)
	return query, args
}
