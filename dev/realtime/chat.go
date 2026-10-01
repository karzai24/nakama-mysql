// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/gorilla/websocket"
	"github.com/heroiclabs/nakama-common/rtapi"
)

type chatMessage struct {
	ID         string `json:"message_id"`
	Channel    string `json:"channel_id"`
	Content    string `json:"content"`
	Code       int    `json:"code"`
	Persistent bool   `json:"persistent"`
	Created    string `json:"create_time"`
	Updated    string `json:"update_time"`
}
type chatHistory struct {
	Messages []chatMessage `json:"messages"`
	Next     string        `json:"next_cursor"`
	Previous string        `json:"prev_cursor"`
	Cache    string        `json:"cacheable_cursor"`
}

func (c *client) history(channel, query string, expected int) (*chatHistory, error) {
	request, _ := http.NewRequest("GET", "http://nakama:7350/v2/channel/"+url.PathEscape(channel)+"?"+query, nil)
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return nil, errors.New("history request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != expected {
		return nil, fmt.Errorf("history expected %d, got %d", expected, response.StatusCode)
	}
	if expected != 200 {
		return nil, nil
	}
	var page chatHistory
	if err = json.NewDecoder(response.Body).Decode(&page); err != nil {
		return nil, err
	}
	return &page, nil
}
func (c *client) joinChat(target string, kind rtapi.ChannelJoin_Type, persist bool) (string, error) {
	if err := c.send("chat-join", "channel_join", map[string]any{"type": int(kind), "target": target, "persistence": persist}); err != nil {
		return "", err
	}
	e, err := c.receive("channel", "chat-join")
	if err != nil {
		return "", err
	}
	var channel struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(e["channel"], &channel); err != nil {
		return "", err
	}
	if channel.ID == "" {
		return "", errors.New("chat join returned no channel")
	}
	return channel.ID, nil
}
func (c *client) leaveChat(channel string) error {
	if err := c.send("chat-leave", "channel_leave", map[string]string{"channel_id": channel}); err != nil {
		return err
	}
	_, err := c.receive("", "chat-leave")
	return err
}

func (c *client) chatAction(action, channel, id, content string, accepted bool) (*chatMessage, error) {
	body := map[string]string{"channel_id": channel}
	if action != "send" {
		body["message_id"] = id
	}
	if action != "remove" {
		body["content"] = content
	}
	cid := "chat-" + action
	if err := c.send(cid, "channel_message_"+action, body); err != nil {
		return nil, err
	}
	key := "channel_message_ack"
	if !accepted {
		key = "error"
	}
	e, err := c.receive(key, cid)
	if err != nil {
		return nil, err
	}
	if !accepted {
		var denied struct {
			Code int `json:"code"`
		}
		if err = json.Unmarshal(e["error"], &denied); err != nil {
			return nil, err
		}
		if denied.Code != int(rtapi.Error_BAD_INPUT) {
			return nil, errors.New("chat rejection was not BAD_INPUT")
		}
		return nil, nil
	}
	var ack chatMessage
	if err = json.Unmarshal(e[key], &ack); err != nil {
		return nil, err
	}
	if ack.ID == "" || ack.Channel != channel {
		return nil, errors.New("chat acknowledgement identity mismatch")
	}
	return &ack, nil
}
func chatEvent(c *client, ack *chatMessage, content string) error {
	e, err := c.receive("channel_message", "")
	if err != nil {
		return err
	}
	var m chatMessage
	if err = json.Unmarshal(e["channel_message"], &m); err != nil {
		return err
	}
	if m.ID != ack.ID || m.Channel != ack.Channel || m.Code != ack.Code || m.Persistent != ack.Persistent || m.Content != content {
		return errors.New("chat event differs from acknowledged operation")
	}
	return nil
}
func chatExchange(a, b *client, channel string, persist bool) error {
	if _, err := a.chatAction("send", channel, "", "{invalid", false); err != nil {
		return err
	}
	content := `{"body":"فارسی 🂡","large":9007199254740993}`
	sent, err := a.chatAction("send", channel, "", content, true)
	if err != nil {
		return err
	}
	if sent.Persistent != persist {
		return errors.New("unexpected chat persistence")
	}
	for _, c := range []*client{a, b} {
		if err := chatEvent(c, sent, content); err != nil {
			return err
		}
	}
	history, err := b.history(channel, "limit=100&forward=true", 200)
	if err != nil {
		return err
	}
	found := false
	for _, m := range history.Messages {
		if m.ID == sent.ID {
			found = true
			if m.Code != 0 || !json.Valid([]byte(m.Content)) {
				return errors.New("invalid persisted chat content")
			}
		}
	}
	if found != persist {
		return errors.New("history did not respect persistence setting")
	}
	if persist {
		if _, err := b.chatAction("update", channel, sent.ID, `{"unauthorized":true}`, false); err != nil {
			return err
		}
		if _, err := b.chatAction("remove", channel, sent.ID, "", false); err != nil {
			return err
		}
	}
	updated, err := a.chatAction("update", channel, sent.ID, `{"edited":true}`, true)
	if err != nil {
		return err
	}
	if updated.Code != 1 {
		return errors.New("unexpected update acknowledgement code")
	}
	for _, c := range []*client{a, b} {
		if err := chatEvent(c, updated, `{"edited":true}`); err != nil {
			return err
		}
	}
	history, err = b.history(channel, "limit=100&forward=true", 200)
	if err != nil {
		return err
	}
	for _, m := range history.Messages {
		if m.ID == sent.ID {
			var content map[string]bool
			if err = json.Unmarshal([]byte(m.Content), &content); err != nil || !content["edited"] {
				return errors.New("history lost message edit")
			}
			if m.Created != updated.Created {
				return errors.New("edit changed persisted creation time")
			}
		}
	}
	removed, err := a.chatAction("remove", channel, sent.ID, "", true)
	if err != nil {
		return err
	}
	if removed.Code != 2 {
		return errors.New("unexpected remove acknowledgement code")
	}
	for _, c := range []*client{a, b} {
		if err := chatEvent(c, removed, `{}`); err != nil {
			return err
		}
	}
	history, err = b.history(channel, "limit=100&forward=true", 200)
	if err != nil {
		return err
	}
	for _, m := range history.Messages {
		if m.ID == sent.ID {
			return errors.New("deleted message remained in history")
		}
	}
	if persist {
		if _, err := a.chatAction("remove", channel, sent.ID, "", false); err != nil {
			return err
		}
	}
	return nil
}
func roomAndDirectChat(a, b *client) error {
	room := "chat-smoke-" + uuid.Must(uuid.NewV4()).String()
	channel, err := a.joinChat(room, rtapi.ChannelJoin_ROOM, true)
	if err != nil {
		return err
	}
	if _, err = b.chatAction("send", channel, "", `{}`, false); err != nil {
		return err
	}
	if _, err = b.joinChat(room, rtapi.ChannelJoin_ROOM, true); err != nil {
		return err
	}
	if err = chatExchange(a, b, channel, true); err != nil {
		return err
	}
	// Rejoin with temporary persistence. Both delivery and edit/remove still work,
	// and no row may appear in HTTP history.
	if err = a.leaveChat(channel); err != nil {
		return err
	}
	if _, err = a.joinChat(room, rtapi.ChannelJoin_ROOM, false); err != nil {
		return err
	}
	if err = chatExchange(a, b, channel, false); err != nil {
		return err
	}
	if err = a.leaveChat(channel); err != nil {
		return err
	}
	if _, err = a.joinChat(room, rtapi.ChannelJoin_ROOM, true); err != nil {
		return err
	}
	ids := []string{}
	for i := 0; i < 3; i++ {
		ack, err := a.chatAction("send", channel, "", `{"page":true}`, true)
		if err != nil {
			return err
		}
		ids = append(ids, ack.ID)
		for _, c := range []*client{a, b} {
			if err := chatEvent(c, ack, `{"page":true}`); err != nil {
				return err
			}
		}
	}
	first, err := a.history(channel, "limit=1&forward=true", 200)
	if err != nil {
		return err
	}
	if len(first.Messages) != 1 || first.Messages[0].ID != ids[0] || first.Next == "" {
		return errors.New("wrong first history page")
	}
	second, err := a.history(channel, "limit=1&forward=true&cursor="+url.QueryEscape(first.Next), 200)
	if err != nil {
		return err
	}
	if len(second.Messages) != 1 || second.Messages[0].ID != ids[1] || second.Previous == "" {
		return errors.New("wrong next history page")
	}
	back, err := a.history(channel, "limit=1&forward=true&cursor="+url.QueryEscape(second.Previous), 200)
	if err != nil {
		return err
	}
	if len(back.Messages) != 1 || back.Messages[0].ID != ids[0] {
		return errors.New("wrong previous history page")
	}
	if _, err = a.history(channel, "forward=false&cursor="+url.QueryEscape(first.Next), 400); err != nil {
		return err
	}
	// Reconnect the same account; persistent history must survive socket lifecycle.
	a.conn.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws://nakama:7350/ws?format=json&status=true", http.Header{"Authorization": []string{"Bearer " + a.token}})
	if err != nil {
		return errors.New("chat reconnect failed")
	}
	a.conn = conn
	a.pending = nil
	if _, err = a.joinChat(room, rtapi.ChannelJoin_ROOM, true); err != nil {
		return err
	}
	recovered, err := a.history(channel, "limit=100&forward=true", 200)
	if err != nil {
		return err
	}
	if len(recovered.Messages) != 3 {
		return errors.New("chat history did not survive reconnect")
	}
	for i, m := range recovered.Messages {
		if m.ID != ids[i] {
			return errors.New("history order changed after reconnect")
		}
	}
	empty, err := a.history(channel, "forward=true&cursor="+url.QueryEscape(recovered.Cache), 200)
	if err != nil {
		return err
	}
	if len(empty.Messages) != 0 || empty.Cache != recovered.Cache {
		return errors.New("history refresh cursor lost its position")
	}
	// Account deletion at the end of the smoke removes these persisted room rows.
	var accountA, accountB struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err = a.api("GET", "/v2/account", nil, &accountA); err != nil {
		return err
	}
	if err = b.api("GET", "/v2/account", nil, &accountB); err != nil {
		return err
	}
	dm, err := a.joinChat(accountB.User.ID, rtapi.ChannelJoin_DIRECT_MESSAGE, true)
	if err != nil {
		return err
	}
	other, err := b.joinChat(accountA.User.ID, rtapi.ChannelJoin_DIRECT_MESSAGE, true)
	if err != nil {
		return err
	}
	if dm != other {
		return errors.New("DM participants joined different channels")
	}
	if err = chatExchange(a, b, dm, true); err != nil {
		return err
	}
	outsider, err := connect()
	if err != nil {
		return err
	}
	defer outsider.conn.Close()
	_, err = outsider.history(dm, "limit=100", 400)
	cleanup := outsider.deleteAccount()
	if err != nil {
		return err
	}
	if cleanup != nil {
		return cleanup
	}
	fmt.Println("PASS persistent room/DM chat, edits/deletes, ownership checks, ephemeral messages, history pagination and reconnect")
	return nil
}
