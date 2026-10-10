// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

func statusPresence(a, b *client) error {
	var account struct {
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := b.api("GET", "/v2/account", nil, &account); err != nil {
		return err
	}
	for i, body := range []map[string]any{
		{"user_ids": []string{account.User.ID}},
		{"usernames": []string{account.User.Username}},
		{"user_ids": []string{account.User.ID}, "usernames": []string{account.User.Username}},
	} {
		cid := fmt.Sprintf("follow-%d", i)
		if err := a.send(cid, "status_follow", body); err != nil {
			return err
		}
		e, err := a.receive("status", cid)
		if err != nil {
			return err
		}
		var status struct {
			Presences []struct {
				UserID string `json:"user_id"`
			} `json:"presences"`
		}
		if err = json.Unmarshal(e["status"], &status); err != nil {
			return err
		}
		if len(status.Presences) != 1 || status.Presences[0].UserID != account.User.ID {
			return errors.New("status follow did not resolve exactly the requested user")
		}
		if err = b.send(cid, "status_update", map[string]string{"status": cid}); err != nil {
			return err
		}
		if _, err = b.receive("", cid); err != nil {
			return err
		}
		// Status tracking may have queued the initial presence before this update.
		// Match the requested user's new value rather than assuming the first event.
		delivered := false
		for attempt := 0; attempt < 8 && !delivered; attempt++ {
			event, err := a.receive("status_presence_event", "")
			if err != nil {
				return err
			}
			var change struct {
				Joins []struct {
					UserID string `json:"user_id"`
					Status string `json:"status"`
				} `json:"joins"`
			}
			if err = json.Unmarshal(event["status_presence_event"], &change); err != nil {
				return err
			}
			for _, presence := range change.Joins {
				if presence.UserID == account.User.ID && presence.Status == cid {
					delivered = true
				}
			}
		}
		if !delivered {
			return errors.New("followed status update was not delivered")
		}
		if err = a.send(cid, "status_unfollow", map[string]any{"user_ids": []string{account.User.ID}}); err != nil {
			return err
		}
		if _, err = a.receive("", cid); err != nil {
			return err
		}
	}
	return nil
}
