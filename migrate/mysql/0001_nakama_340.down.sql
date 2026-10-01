-- Copyright 2018-2026 The Nakama Authors. Licensed under the Apache License, Version 2.0.
-- Copyright 2026 The nakama-mysql contributors. Apache-2.0.
-- MySQL adaptation of Nakama v3.40.0 schema; upstream PostgreSQL migrations stay unchanged.
-- Separate statements use an explicit delimiter so trigger bodies remain intact.
DROP TABLE `wallet_ledger`;
-- statement-break
DROP TABLE `users_notes`;
-- statement-break
DROP TABLE `user_tombstone`;
-- statement-break
DROP TABLE `user_edge`;
-- statement-break
DROP TABLE `user_device`;
-- statement-break
DROP TABLE `subscription`;
-- statement-break
DROP TABLE `storage`;
-- statement-break
DROP TABLE `setting`;
-- statement-break
DROP TABLE `purchase`;
-- statement-break
DROP TABLE `notification`;
-- statement-break
DROP TABLE `message`;
-- statement-break
DROP TABLE `leaderboard_record`;
-- statement-break
DROP TABLE `groups`;
-- statement-break
DROP TABLE `group_edge`;
-- statement-break
DROP TABLE `console_user`;
-- statement-break
DROP TABLE `console_audit_log`;
-- statement-break
DROP TABLE `console_acl_template`;
-- statement-break
DROP TABLE `leaderboard`;
-- statement-break
DROP TABLE `users`;
