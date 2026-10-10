// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"go.uber.org/zap"
)

const mysqlFriendColumns = "SELECT id, username, display_name, avatar_url, lang_tag, location, timezone, users.metadata, create_time, users.update_time, user_edge.update_time, state, `position`, facebook_id, google_id, gamecenter_id, steam_id, facebook_instant_game_id, apple_id, user_edge.metadata FROM users JOIN user_edge ON id = destination_id WHERE source_id = ?"

type mysqlFriendEdge struct {
	state    int
	metadata string
}

func lockFriendOwnerMySQL(ctx context.Context, tx *sql.Tx, owner uuid.UUID) error {
	if owner == uuid.Nil {
		return errors.New("system user cannot own friendships")
	}
	var id string
	err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", owner).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAccountNotFound
	}
	return err
}

// Every friendship mutation locks both users before their edges/counters, sharing
// account deletion's ownership locks. Reciprocal operations can deadlock; the
// common native transaction runner retries only after a confirmed rollback.
func lockFriendPeersMySQL(ctx context.Context, tx *sql.Tx, owner uuid.UUID, ids []string) ([]string, error) {
	canonical, err := canonicalUserIDsMySQL(ids)
	if err != nil {
		return nil, err
	}
	unique := map[string]bool{}
	for _, id := range canonical {
		if id != owner.String() && id != uuid.Nil.String() {
			unique[id] = true
		}
	}
	ordered := make([]string, 0, len(unique))
	for id := range unique {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	found := make([]string, 0, len(ordered))
	for _, id := range ordered {
		var locked string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", id).Scan(&locked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		found = append(found, id)
	}
	return found, nil
}

func mutateFriendsMySQL(ctx context.Context, db *sql.DB, owner uuid.UUID, ids []string, action, metadata string) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	if metadata == "" {
		metadata = "{}"
	}
	if (action == "add" || action == "metadata") && !json.Valid([]byte(metadata)) {
		return nil, errors.New("invalid friend metadata JSON")
	}
	var notifications map[string]bool
	err := executeInTxMySQL(ctx, db, func(tx *sql.Tx) error {
		notifications = map[string]bool{}
		if err := lockFriendOwnerMySQL(ctx, tx, owner); err != nil {
			return err
		}
		peers, err := lockFriendPeersMySQL(ctx, tx, owner, ids)
		if err != nil {
			return err
		}
		for _, peer := range peers {
			changed, accepted, err := mutateFriendPairMySQL(ctx, tx, owner.String(), peer, action, metadata)
			if err != nil {
				return err
			}
			if changed {
				notifications[peer] = accepted
			}
		}
		return nil
	})
	return notifications, err
}

func friendCountMySQL(ctx context.Context, tx *sql.Tx, owner string, delta int) error {
	_, err := tx.ExecContext(ctx, "UPDATE users SET edge_count = edge_count + ?, update_time = CURRENT_TIMESTAMP(6) WHERE id = ?", delta, owner)
	return err
}

func newFriendPositionMySQL(ctx context.Context, tx *sql.Tx, a, b string) (int64, error) {
	var last int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(`position`),0) FROM user_edge WHERE source_id IN (?,?)", a, b).Scan(&last); err != nil {
		return 0, err
	}
	now := time.Now().UTC().UnixNano()
	if last >= now {
		if last == math.MaxInt64 {
			return 0, errors.New("friend position exhausted")
		}
		now = last + 1
	}
	return now, nil
}

// Match PostgreSQL JSONB ||: shallow object merge, keeping JSON null; otherwise
// concatenate arrays with scalar/object operands wrapped as single elements.
// JSON_MERGE_PATCH would recursively merge objects and delete null-valued keys.
func mergeFriendMetadataMySQL(left, right string) (string, error) {
	decode := func(s string) (any, error) {
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var v any
		err := d.Decode(&v)
		return v, err
	}
	a, err := decode(left)
	if err != nil {
		return "", err
	}
	b, err := decode(right)
	if err != nil {
		return "", err
	}
	var merged any
	ma, aObject := a.(map[string]any)
	mb, bObject := b.(map[string]any)
	if aObject && bObject {
		for k, v := range mb {
			ma[k] = v
		}
		merged = ma
	} else {
		aa, ok := a.([]any)
		if !ok {
			aa = []any{a}
		}
		bb, ok := b.([]any)
		if !ok {
			bb = []any{b}
		}
		merged = append(aa, bb...)
	}
	data, err := json.Marshal(merged)
	return string(data), err
}

func mutateFriendPairMySQL(ctx context.Context, tx *sql.Tx, owner, peer, action, metadata string) (changed, accepted bool, err error) {
	rows, err := tx.QueryContext(ctx, "SELECT source_id,state,metadata FROM user_edge WHERE (source_id = ? AND destination_id = ?) OR (source_id = ? AND destination_id = ?)", owner, peer, peer, owner)
	if err != nil {
		return false, false, err
	}
	edges := map[string]mysqlFriendEdge{}
	for rows.Next() {
		var source string
		var edge mysqlFriendEdge
		if err := rows.Scan(&source, &edge.state, &edge.metadata); err != nil {
			rows.Close()
			return false, false, err
		}
		edges[source] = edge
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	own, hasOwn := edges[owner]
	other, hasOther := edges[peer]
	remove := func(source, destination string) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM user_edge WHERE source_id = ? AND destination_id = ?", source, destination); err != nil {
			return err
		}
		return friendCountMySQL(ctx, tx, source, -1)
	}
	insert := func(source, destination string, state int, position int64, value string) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO user_edge (source_id,destination_id,state,`position`,metadata) VALUES (?,?,?,?,CAST(? AS JSON))", source, destination, state, position, value); err != nil {
			return err
		}
		return friendCountMySQL(ctx, tx, source, 1)
	}
	switch action {
	case "metadata":
		if hasOwn {
			_, err = tx.ExecContext(ctx, "UPDATE user_edge SET metadata = CAST(? AS JSON) WHERE source_id = ? AND destination_id = ?", metadata, owner, peer)
		}
		return false, false, err
	case "delete":
		removed := 0
		if hasOwn {
			if err = remove(owner, peer); err != nil {
				return false, false, err
			}
			removed++
		}
		if hasOther && other.state != 3 {
			if err = remove(peer, owner); err != nil {
				return false, false, err
			}
			removed++
		}
		return removed == 2, false, nil
	case "block":
		if hasOwn {
			_, err = tx.ExecContext(ctx, "UPDATE user_edge SET state = 3, update_time = CURRENT_TIMESTAMP(6) WHERE source_id = ? AND destination_id = ?", owner, peer)
		} else {
			var position int64
			position, err = newFriendPositionMySQL(ctx, tx, owner, peer)
			if err == nil {
				err = insert(owner, peer, 3, position, "{}")
			}
		}
		if err != nil {
			return false, false, err
		}
		if hasOther && other.state != 3 {
			if err = remove(peer, owner); err != nil {
				return false, false, err
			}
		}
		return true, false, nil
	case "add", "import":
		if (hasOwn && own.state == 3) || (hasOther && other.state == 3) {
			return false, false, nil
		}
		if !hasOwn && !hasOther {
			position, err := newFriendPositionMySQL(ctx, tx, owner, peer)
			if err != nil {
				return false, false, err
			}
			ownState, otherState, otherMetadata := 1, 2, "{}"
			if action == "import" {
				ownState, otherState, otherMetadata = 0, 0, metadata
			}
			if err := insert(owner, peer, ownState, position, metadata); err != nil {
				return false, false, err
			}
			if err := insert(peer, owner, otherState, position, otherMetadata); err != nil {
				return false, false, err
			}
			return true, false, nil
		}
		if !hasOwn || !hasOther {
			return false, false, errors.New("incomplete friendship edges")
		}
		if own.state == 0 && other.state == 0 {
			return false, false, nil
		}
		if own.state == 1 && other.state == 2 && action == "add" {
			return false, false, nil
		}
		if !((own.state == 1 && other.state == 2) || (own.state == 2 && other.state == 1)) {
			return false, false, errors.New("invalid friendship states")
		}
		merged, err := mergeFriendMetadataMySQL(own.metadata, metadata)
		if err != nil {
			return false, false, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE user_edge SET state = 0, metadata = CAST(? AS JSON), update_time = CURRENT_TIMESTAMP(6) WHERE source_id = ? AND destination_id = ?", merged, owner, peer); err != nil {
			return false, false, err
		}
		if action == "import" {
			other.metadata, err = mergeFriendMetadataMySQL(other.metadata, metadata)
			if err != nil {
				return false, false, err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE user_edge SET state = 0, metadata = CAST(? AS JSON), update_time = CURRENT_TIMESTAMP(6) WHERE source_id = ? AND destination_id = ?", other.metadata, peer, owner); err != nil {
			return false, false, err
		}
		return true, true, nil
	default:
		return false, false, errors.New("unknown friendship mutation")
	}
}

func listFriendsOfFriendsMySQL(ctx context.Context, logger *zap.Logger, db *sql.DB, registry StatusRegistry, owner uuid.UUID, limit int, cursor string) (*api.FriendsOfFriendsList, error) {
	var incoming *friendsOfFriendsListCursor
	if cursor != "" {
		data, err := base64.URLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, runtime.ErrFriendInvalidCursor
		}
		incoming = &friendsOfFriendsListCursor{}
		if err := gob.NewDecoder(bytes.NewReader(data)).Decode(incoming); err != nil {
			return nil, runtime.ErrFriendInvalidCursor
		}
		ids, err := canonicalUserIDsMySQL([]string{incoming.SourceId, incoming.DestinationId})
		if err != nil {
			return nil, runtime.ErrFriendInvalidCursor
		}
		incoming.SourceId, incoming.DestinationId = ids[0], ids[1]
	}
	query := `SELECT peer.source_id,peer.destination_id FROM user_edge direct
        JOIN user_edge peer ON peer.source_id = direct.destination_id AND peer.state = 0
        WHERE direct.source_id = ? AND direct.state = 0 AND peer.destination_id <> ?
        AND NOT EXISTS (SELECT 1 FROM user_edge existing WHERE existing.source_id = ? AND existing.destination_id = peer.destination_id AND existing.state = 0)
        AND NOT EXISTS (SELECT 1 FROM user_edge blocked WHERE blocked.state = 3 AND ((blocked.source_id = ? AND blocked.destination_id = peer.destination_id) OR (blocked.source_id = peer.destination_id AND blocked.destination_id = ?)))`
	args := []any{owner, owner, owner, owner, owner}
	if incoming != nil {
		query += " AND (peer.source_id,peer.destination_id) >= (?,?)"
		args = append(args, incoming.SourceId, incoming.DestinationId)
	}
	query += " ORDER BY peer.source_id,peer.destination_id LIMIT ?"
	args = append(args, limit+1)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	pairs := []friendsOfFriendsListCursor{}
	ids := []string{}
	outgoing := ""
	for rows.Next() {
		var pair friendsOfFriendsListCursor
		if err := rows.Scan(&pair.SourceId, &pair.DestinationId); err != nil {
			rows.Close()
			return nil, err
		}
		if len(pairs) == limit {
			var buffer bytes.Buffer
			if err := gob.NewEncoder(&buffer).Encode(pair); err != nil {
				rows.Close()
				return nil, err
			}
			outgoing = base64.URLEncoding.EncodeToString(buffer.Bytes())
			break
		}
		pairs = append(pairs, pair)
		ids = append(ids, pair.DestinationId)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := &api.FriendsOfFriendsList{FriendsOfFriends: []*api.FriendsOfFriendsList_FriendOfFriend{}, Cursor: outgoing}
	if len(ids) == 0 {
		return result, nil
	}
	users, err := GetUsers(ctx, logger, db, registry, ids, nil, nil)
	if err != nil {
		return nil, err
	}
	byID := map[string]*api.User{}
	for _, u := range users.Users {
		byID[u.Id] = u
	}
	for _, pair := range pairs {
		if u := byID[pair.DestinationId]; u != nil {
			result.FriendsOfFriends = append(result.FriendsOfFriends, &api.FriendsOfFriendsList_FriendOfFriend{Referrer: pair.SourceId, User: u})
		}
	}
	return result, nil
}
