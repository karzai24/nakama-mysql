-- Copyright 2026 The nakama-mysql contributors. Apache-2.0.
ALTER TABLE user_device ADD COLUMN provider VARCHAR(128) NOT NULL DEFAULT '';
