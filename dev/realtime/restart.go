// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/gorilla/websocket"
	"github.com/heroiclabs/nakama-common/rtapi"
)

// This optional probe retains its token only in memory while another terminal
// recreates the local Nakama service. It never controls Docker or touches volumes.
func restartChatProbe() error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer func() { _ = c.deleteAccount(); c.conn.Close() }()
	room := "restart-chat-" + uuid.Must(uuid.NewV4()).String()
	channel, err := c.joinChat(room, rtapi.ChannelJoin_ROOM, true)
	if err != nil {
		return err
	}
	ack, err := c.chatAction("send", channel, "", `{"survives_restart":true}`, true)
	if err != nil {
		return err
	}
	if err = chatEvent(c, ack, `{"survives_restart":true}`); err != nil {
		return err
	}
	fmt.Println("READY: message persisted; recreate only the local Nakama service now.")
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	for {
		_, _, err = c.conn.ReadMessage()
		if err != nil {
			break
		}
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return errors.New("no server restart observed within 60 seconds")
	}
	c.conn.Close()
	deadline := time.Now().Add(30 * time.Second)
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		conn, _, dialErr := dialer.Dial("ws://nakama:7350/ws?format=json&status=true", http.Header{"Authorization": []string{"Bearer " + c.token}})
		if dialErr == nil {
			c.conn = conn
			c.pending = nil
			err = nil
			break
		}
		err = errors.New("could not reconnect after server restart")
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	if _, err = c.joinChat(room, rtapi.ChannelJoin_ROOM, true); err != nil {
		return err
	}
	page, err := c.history(channel, "limit=10&forward=true", 200)
	if err != nil {
		return err
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != ack.ID || !page.Messages[0].Persistent {
		return errors.New("persisted message missing after restart")
	}
	fmt.Println("PASS persistent chat history survived Nakama recreation and client reconnect")
	return nil
}
