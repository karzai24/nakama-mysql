// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
// Local Compose smoke only: two real clients, party join, matchmaking, relayed match.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/gorilla/websocket"
	"github.com/heroiclabs/nakama-common/rtapi"
)

type envelope map[string]json.RawMessage
type client struct {
	token   string
	conn    *websocket.Conn
	pending []envelope
}

func connect() (*client, error) {
	key := os.Getenv("NAKAMA_SERVER_KEY")
	if key == "" {
		return nil, errors.New("requires local Compose .env")
	}
	body, _ := json.Marshal(map[string]string{"id": "realtime-smoke-" + uuid.Must(uuid.NewV4()).String()})
	request, _ := http.NewRequest("POST", "http://nakama:7350/v2/account/authenticate/device?create=true", bytes.NewReader(body))
	request.SetBasicAuth(key, "")
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("device login returned %d", response.StatusCode)
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		return nil, err
	}
	headers := http.Header{"Authorization": []string{"Bearer " + session.Token}}
	conn, _, err := websocket.DefaultDialer.Dial("ws://nakama:7350/ws?format=json&status=true", headers)
	if err != nil {
		return nil, errors.New("WebSocket connection failed")
	}
	return &client{conn: conn, token: session.Token}, nil
}

func (c *client) receive(key, cid string) (envelope, error) {
	matches := func(e envelope) bool {
		var id string
		_ = json.Unmarshal(e["cid"], &id)
		return (key == "" || e[key] != nil) && (cid == "" || id == cid)
	}
	for i, e := range c.pending {
		if matches(e) {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return e, nil
		}
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(35 * time.Second))
	for len(c.pending) < 32 {
		var e envelope
		if err := c.conn.ReadJSON(&e); err != nil {
			return nil, err
		}
		if e["error"] != nil && key != "error" {
			return nil, errors.New("server returned a realtime error")
		}
		if matches(e) {
			return e, nil
		}
		c.pending = append(c.pending, e)
	}
	return nil, errors.New("too many unexpected realtime messages")
}

func (c *client) send(cid, key string, body any) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(map[string]any{"cid": cid, key: body})
}

// Read the server close frame while deletion is in flight so the real client
// acknowledges it, instead of making the server wait for its close-ack timeout.
func (c *client) deleteAccount() error {
	done := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest("DELETE", "http://nakama:7350/v2/account", nil)
		request.Header.Set("Authorization", "Bearer "+c.token)
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
		if err != nil {
			done <- errors.New("account deletion request failed")
			return
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			done <- fmt.Errorf("account deletion returned %d", response.StatusCode)
			return
		}
		done <- nil
	}()
	_ = c.conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			requestErr := <-done
			if requestErr != nil {
				return requestErr
			}
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				return errors.New("deletion did not close the WebSocket normally")
			}
			return nil
		}
	}
}

// Exercise real group-channel authorization and delivery after membership commits.
func (c *client) api(method, path string, body any, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(method, "http://nakama:7350"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return errors.New("group API request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("group API %s %s returned %d", method, strings.Split(path, "?")[0], response.StatusCode)
	}
	if result != nil {
		return json.NewDecoder(response.Body).Decode(result)
	}
	return nil
}
func groupChannels(a, b *client) error {
	var account struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := b.api("GET", "/v2/account", nil, &account); err != nil {
		return err
	}
	var group struct {
		ID string `json:"id"`
	}
	if err := a.api("POST", "/v2/group", map[string]any{"name": "realtime-group-" + uuid.Must(uuid.NewV4()).String(), "open": false}, &group); err != nil {
		return err
	}
	if group.ID == "" || account.User.ID == "" {
		return errors.New("missing group or user ID")
	}
	path := "/v2/group/" + group.ID
	// Always remove this run's group, including when a channel assertion fails.
	defer a.api("DELETE", path, nil, nil)
	var groupChannel string
	join := func(c *client, cid string, allowed bool) error {
		if err := c.send(cid, "channel_join", map[string]any{"type": 3, "target": group.ID, "persistence": true}); err != nil {
			return err
		}
		key := "channel"
		if !allowed {
			key = "error"
		}
		e, err := c.receive(key, cid)
		if err != nil {
			return err
		}
		if !allowed {
			var denied struct {
				Code int `json:"code"`
			}
			if err = json.Unmarshal(e["error"], &denied); err != nil || denied.Code != int(rtapi.Error_BAD_INPUT) {
				return errors.New("group channel denial was not BAD_INPUT")
			}
		} else {
			var joined struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(e["channel"], &joined); err != nil {
				return err
			}
			if joined.ID == "" || (groupChannel != "" && groupChannel != joined.ID) {
				return errors.New("group members joined different channels")
			}
			groupChannel = joined.ID
		}
		return nil
	}
	event := func(c *client, code int) error {
		e, err := c.receive("channel_message", "")
		if err != nil {
			return err
		}
		var m struct {
			Code       int    `json:"code"`
			GroupID    string `json:"group_id"`
			SenderID   string `json:"sender_id"`
			Persistent bool   `json:"persistent"`
		}
		if err = json.Unmarshal(e["channel_message"], &m); err != nil {
			return err
		}
		if m.Code != code || m.GroupID != group.ID || m.SenderID != account.User.ID || !m.Persistent {
			return errors.New("unexpected group membership channel event")
		}
		return nil
	}
	if err := join(a, "owner-channel", true); err != nil {
		return err
	}
	if err := join(b, "nonmember-channel", false); err != nil {
		return err
	}
	if err := b.api("POST", path+"/join", nil, nil); err != nil {
		return err
	}
	if err := join(b, "request-channel", false); err != nil {
		return err
	}
	if err := a.api("POST", path+"/add?user_ids="+account.User.ID, nil, nil); err != nil {
		return err
	}
	if err := event(a, 4); err != nil {
		return err
	}
	if err := join(b, "member-channel", true); err != nil {
		return err
	}
	if err := chatExchange(a, b, groupChannel, true); err != nil {
		return err
	}
	if err := a.api("POST", path+"/promote?user_ids="+account.User.ID, nil, nil); err != nil {
		return err
	}
	if err := event(a, 7); err != nil {
		return err
	}
	if err := event(b, 7); err != nil {
		return err
	}
	if err := a.api("POST", path+"/ban?user_ids="+account.User.ID, nil, nil); err != nil {
		return err
	}
	if err := event(a, 8); err != nil {
		return err
	}
	if err := event(b, 8); err != nil {
		return err
	}
	// The owner must observe the banned member leaving the actual channel stream.
	removed := false
	for i := 0; i < 4; i++ {
		e, err := a.receive("channel_presence_event", "")
		if err != nil {
			return err
		}
		var change struct {
			Leaves []struct {
				UserID string `json:"user_id"`
			} `json:"leaves"`
		}
		if err = json.Unmarshal(e["channel_presence_event"], &change); err != nil {
			return err
		}
		for _, p := range change.Leaves {
			if p.UserID == account.User.ID {
				removed = true
			}
		}
		if removed {
			break
		}
	}
	if !removed {
		return errors.New("banned member remained in group channel")
	}
	if err := join(b, "banned-channel", false); err != nil {
		return err
	}
	if _, err := b.history(groupChannel, "limit=100", 400); err != nil {
		return err
	}
	if _, err := b.chatAction("send", groupChannel, "", `{}`, false); err != nil {
		return err
	}
	fmt.Println("PASS group-channel membership authorization, persisted add/promote/ban events and presence eviction")
	return nil
}

func run() (runErr error) {
	a, err := connect()
	if err != nil {
		return err
	}
	defer func() { a.conn.Close() }()
	b, err := connect()
	if err != nil {
		return err
	}
	defer b.conn.Close()
	defer func() {
		if runErr != nil {
			for _, c := range []*client{a, b} {
				_ = c.deleteAccount()
			}
		}
	}()
	if err := statusPresence(a, b); err != nil {
		return err
	}
	if err := groupChannels(a, b); err != nil {
		return err
	}
	if err := roomAndDirectChat(a, b); err != nil {
		return err
	}
	if err := a.send("create-party", "party_create", map[string]any{"open": true, "max_size": 2}); err != nil {
		return err
	}
	e, err := a.receive("party", "create-party")
	if err != nil {
		return err
	}
	var party struct {
		ID string `json:"party_id"`
	}
	if err := json.Unmarshal(e["party"], &party); err != nil || party.ID == "" {
		return errors.New("party creation returned no ID")
	}
	if err := b.send("join-party", "party_join", map[string]string{"party_id": party.ID}); err != nil {
		return err
	}
	joined, err := b.receive("party", "")
	if err != nil {
		return err
	}
	var joinedParty struct {
		ID        string            `json:"party_id"`
		Presences []json.RawMessage `json:"presences"`
	}
	if err := json.Unmarshal(joined["party"], &joinedParty); err != nil || joinedParty.ID != party.ID || len(joinedParty.Presences) != 2 {
		return errors.New("party did not contain both clients")
	}
	if err := a.send("close-party", "party_close", map[string]string{"party_id": party.ID}); err != nil {
		return err
	}
	if _, err := a.receive("", "close-party"); err != nil {
		return err
	}
	fmt.Println("PASS two authenticated WebSocket clients, party create/join/close")

	runID := strings.ReplaceAll(uuid.Must(uuid.NewV4()).String(), "-", "")
	for _, c := range []*client{a, b} {
		request := map[string]any{"min_count": 2, "max_count": 2, "query": "+properties.smoke:" + runID, "string_properties": map[string]string{"smoke": runID}}
		if err := c.send("queue", "matchmaker_add", request); err != nil {
			return err
		}
		if _, err := c.receive("matchmaker_ticket", "queue"); err != nil {
			return err
		}
	}
	matchID := ""
	for _, c := range []*client{a, b} {
		matched, err := c.receive("matchmaker_matched", "")
		if err != nil {
			return err
		}
		var result struct {
			Token string            `json:"token"`
			Users []json.RawMessage `json:"users"`
		}
		if err := json.Unmarshal(matched["matchmaker_matched"], &result); err != nil || len(result.Users) != 2 || result.Token == "" {
			return errors.New("matchmaker did not pair both clients")
		}
		if err := c.send("join-match", "match_join", map[string]string{"token": result.Token}); err != nil {
			return err
		}
		joined, err := c.receive("match", "join-match")
		if err != nil {
			return err
		}
		var match struct {
			ID string `json:"match_id"`
		}
		if err := json.Unmarshal(joined["match"], &match); err != nil || match.ID == "" {
			return errors.New("match join returned no ID")
		}
		if matchID != "" && match.ID != matchID {
			return errors.New("clients joined different matches")
		}
		matchID = match.ID
	}
	fmt.Println("PASS matchmaking pair and both clients joining the same relayed match")
	for _, c := range []*client{a, b} {
		if err := c.deleteAccount(); err != nil {
			return err
		}
	}
	fmt.Println("PASS deleting both accounts disconnects their live WebSocket sessions")
	return nil
}

func main() {
	var err error
	if os.Getenv("NAKAMA_CHAT_RESTART_PROBE") == "true" {
		err = restartChatProbe()
	} else {
		err = run()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Realtime smoke failed:", err)
		os.Exit(1)
	}
}
