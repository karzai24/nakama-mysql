-- Copyright 2018-2026 The Nakama Authors. Licensed under the Apache License, Version 2.0.
-- Copyright 2026 The nakama-mysql contributors. Apache-2.0.
-- MySQL adaptation of Nakama v3.40.0 schema; upstream PostgreSQL migrations stay unchanged.
-- Separate statements use an explicit delimiter so trigger bodies remain intact.
CREATE TABLE `users` (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    username VARCHAR(128) NOT NULL,
    display_name VARCHAR(255),
    avatar_url VARCHAR(512),
    lang_tag VARCHAR(18) DEFAULT 'en' NOT NULL,
    location VARCHAR(255),
    timezone VARCHAR(255),
    metadata JSON DEFAULT ('{}') NOT NULL,
    wallet JSON DEFAULT ('{}') NOT NULL,
    email VARCHAR(255),
    password BLOB,
    facebook_id VARCHAR(128),
    google_id VARCHAR(128),
    gamecenter_id VARCHAR(128),
    steam_id VARCHAR(128),
    custom_id VARCHAR(128),
    edge_count integer DEFAULT 0 NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    verify_time DATETIME(6) DEFAULT '1970-01-01 00:00:00' NOT NULL,
    disable_time DATETIME(6) DEFAULT '1970-01-01 00:00:00' NOT NULL,
    facebook_instant_game_id VARCHAR(128),
    apple_id VARCHAR(128),
    CONSTRAINT users_edge_count_check CHECK ((edge_count >= 0)),
    CONSTRAINT users_password_check CHECK ((length(password) < 32000)),
    CONSTRAINT users_apple_id_key UNIQUE (apple_id),
    CONSTRAINT users_custom_id_key UNIQUE (custom_id),
    CONSTRAINT users_email_key UNIQUE (email),
    CONSTRAINT users_facebook_id_key UNIQUE (facebook_id),
    CONSTRAINT users_facebook_instant_game_id_key UNIQUE (facebook_instant_game_id),
    CONSTRAINT users_gamecenter_id_key UNIQUE (gamecenter_id),
    CONSTRAINT users_google_id_key UNIQUE (google_id),
    CONSTRAINT users_pkey PRIMARY KEY (id),
    CONSTRAINT users_steam_id_key UNIQUE (steam_id),
    CONSTRAINT users_username_key UNIQUE (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `leaderboard` (
    id VARCHAR(128) NOT NULL,
    authoritative boolean DEFAULT false NOT NULL,
    sort_order smallint DEFAULT 1 NOT NULL,
    operator smallint DEFAULT 0 NOT NULL,
    reset_schedule VARCHAR(64),
    metadata JSON DEFAULT ('{}') NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    category smallint DEFAULT 0 NOT NULL,
    description VARCHAR(255) DEFAULT '' NOT NULL,
    duration integer DEFAULT 0 NOT NULL,
    end_time DATETIME(6) DEFAULT '1970-01-01 00:00:00' NOT NULL,
    join_required boolean DEFAULT false NOT NULL,
    max_size integer DEFAULT 100000000 NOT NULL,
    max_num_score integer DEFAULT 1000000 NOT NULL,
    title VARCHAR(255) DEFAULT '' NOT NULL,
    size integer DEFAULT 0 NOT NULL,
    start_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    enable_ranks boolean DEFAULT true,
    CONSTRAINT leaderboard_category_check CHECK ((category >= 0)),
    CONSTRAINT leaderboard_duration_check CHECK ((duration >= 0)),
    CONSTRAINT leaderboard_max_num_score_check CHECK ((max_num_score > 0)),
    CONSTRAINT leaderboard_max_size_check CHECK ((max_size > 0)),
    CONSTRAINT leaderboard_pkey PRIMARY KEY (id),
    INDEX `duration_start_time_end_time_category_idx` (duration, start_time, end_time DESC, category),
    INDEX `leaderboard_create_time_id_idx` (create_time, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `console_acl_template` (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(64) NOT NULL,
    description VARCHAR(64) DEFAULT '' NOT NULL,
    acl JSON DEFAULT ('{}') NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    CONSTRAINT console_acl_template_name_check CHECK ((CHAR_LENGTH((name)) > 0)),
    CONSTRAINT console_acl_template_pkey PRIMARY KEY (id),
    CONSTRAINT template_name_uniq UNIQUE (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `console_audit_log` (
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    console_user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    console_username LONGTEXT NOT NULL,
    email LONGTEXT NOT NULL,
    action LONGTEXT NOT NULL,
    resource LONGTEXT NOT NULL,
    message LONGTEXT NOT NULL,
    metadata JSON DEFAULT ('{}') NOT NULL,
    PRIMARY KEY (id),
    INDEX console_audit_log_time_id_idx (create_time, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `console_user` (
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    disable_time DATETIME(6) DEFAULT '1970-01-01 00:00:00' NOT NULL,
    email VARCHAR(255) NOT NULL,
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    metadata JSON DEFAULT ('{}') NOT NULL,
    password BLOB,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    username VARCHAR(128) NOT NULL,
    mfa_secret BLOB,
    mfa_recovery_codes BLOB,
    mfa_required boolean DEFAULT false,
    acl JSON DEFAULT ('{"admin": false}') NOT NULL,
    CONSTRAINT console_user_password_check CHECK ((length(password) < 32000)),
    CONSTRAINT console_user_email_uniq UNIQUE (email),
    CONSTRAINT console_user_pkey PRIMARY KEY (id),
    CONSTRAINT console_user_username_uniq UNIQUE (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `group_edge` (
    source_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `position` bigint NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    destination_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state smallint DEFAULT 0 NOT NULL,
    CONSTRAINT group_edge_destination_id_check CHECK ((destination_id <> '00000000-0000-0000-0000-000000000000')),
    CONSTRAINT group_edge_source_id_check CHECK ((source_id <> '00000000-0000-0000-0000-000000000000')),
    CONSTRAINT group_edge_pkey PRIMARY KEY (source_id, state, `position`),
    CONSTRAINT group_edge_source_id_destination_id_key UNIQUE (source_id, destination_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `groups` (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    creator_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(255) NOT NULL,
    description VARCHAR(255),
    avatar_url VARCHAR(512),
    lang_tag VARCHAR(18) DEFAULT 'en' NOT NULL,
    metadata JSON DEFAULT ('{}') NOT NULL,
    state smallint DEFAULT 0 NOT NULL,
    edge_count integer DEFAULT 0 NOT NULL,
    max_count integer DEFAULT 100 NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    disable_time DATETIME(6) DEFAULT '1970-01-01 00:00:00' NOT NULL,
    CONSTRAINT groups_check CHECK (((edge_count >= 1) AND (edge_count <= max_count))),
    CONSTRAINT groups_max_count_check CHECK ((max_count >= 1)),
    CONSTRAINT groups_state_check CHECK ((state >= 0)),
    CONSTRAINT groups_id_key UNIQUE (id),
    CONSTRAINT groups_name_key UNIQUE (name),
    CONSTRAINT groups_pkey PRIMARY KEY (disable_time, lang_tag, edge_count, id),
    INDEX `edge_count_update_time_id_idx` (disable_time, edge_count, update_time, id),
    INDEX `update_time_edge_count_id_idx` (disable_time, update_time, edge_count, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `leaderboard_record` (
    leaderboard_id VARCHAR(128) NOT NULL,
    owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    username VARCHAR(128),
    score bigint DEFAULT 0 NOT NULL,
    subscore bigint DEFAULT 0 NOT NULL,
    num_score integer DEFAULT 1 NOT NULL,
    metadata JSON DEFAULT ('{}') NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    expiry_time DATETIME(6) DEFAULT '1970-01-01 00:00:00' NOT NULL,
    max_num_score integer DEFAULT 1000000 NOT NULL,
    CONSTRAINT leaderboard_record_max_num_score_check CHECK ((max_num_score > 0)),
    CONSTRAINT leaderboard_record_num_score_check CHECK ((num_score >= 0)),
    CONSTRAINT leaderboard_record_score_check CHECK ((score >= 0)),
    CONSTRAINT leaderboard_record_subscore_check CHECK ((subscore >= 0)),
    CONSTRAINT leaderboard_record_owner_id_leaderboard_id_expiry_time_key UNIQUE (owner_id, leaderboard_id, expiry_time),
    CONSTRAINT leaderboard_record_pkey PRIMARY KEY (leaderboard_id, expiry_time, score, subscore, owner_id),
    CONSTRAINT leaderboard_record_leaderboard_id_fkey FOREIGN KEY (leaderboard_id) REFERENCES leaderboard(id) ON DELETE CASCADE,
    INDEX `owner_id_expiry_time_leaderboard_id_idx` (owner_id, expiry_time, leaderboard_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `message` (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code smallint DEFAULT 0 NOT NULL,
    sender_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    username VARCHAR(128) NOT NULL,
    stream_mode smallint NOT NULL,
    stream_subject CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    stream_descriptor CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    stream_label VARCHAR(128) NOT NULL,
    content JSON DEFAULT ('{}') NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    CONSTRAINT message_id_key UNIQUE (id),
    CONSTRAINT message_pkey PRIMARY KEY (stream_mode, stream_subject, stream_descriptor, stream_label, create_time, id),
    CONSTRAINT message_sender_id_id_key UNIQUE (sender_id, id),
    CONSTRAINT message_sender_id_fkey FOREIGN KEY (sender_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `notification` (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    subject VARCHAR(255) NOT NULL,
    content JSON DEFAULT ('{}') NOT NULL,
    code smallint NOT NULL,
    sender_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    CONSTRAINT notification_id_key UNIQUE (id),
    CONSTRAINT notification_pkey PRIMARY KEY (user_id, create_time, id),
    CONSTRAINT notification_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `purchase` (
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    environment smallint DEFAULT 0 NOT NULL,
    product_id VARCHAR(512) NOT NULL,
    purchase_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    raw_response JSON DEFAULT ('{}') NOT NULL,
    store smallint DEFAULT 0 NOT NULL,
    transaction_id VARCHAR(512) NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin DEFAULT '00000000-0000-0000-0000-000000000000' NOT NULL,
    refund_time DATETIME(6) DEFAULT '1970-01-01 00:00:00' NOT NULL,
    CONSTRAINT purchase_transaction_id_check CHECK ((CHAR_LENGTH((transaction_id)) > 0)),
    CONSTRAINT purchase_pkey PRIMARY KEY (transaction_id),
    CONSTRAINT purchase_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT,
    INDEX `purchase_time_user_id_transaction_id_idx` (purchase_time DESC, user_id DESC, transaction_id DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `setting` (
    name VARCHAR(64) NOT NULL,
    value JSON DEFAULT ('{}') NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    CONSTRAINT setting_name_check CHECK ((CHAR_LENGTH((name)) > 0)),
    CONSTRAINT setting_name_uniq PRIMARY KEY (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `storage` (
    collection VARCHAR(128) NOT NULL,
    `key` VARCHAR(128) NOT NULL,
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    value JSON DEFAULT ('{}') NOT NULL,
    version VARCHAR(32) NOT NULL,
    `read` smallint DEFAULT 1 NOT NULL,
    `write` smallint DEFAULT 1 NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    CONSTRAINT storage_read_check CHECK ((`read` >= 0)),
    CONSTRAINT storage_write_check CHECK ((`write` >= 0)),
    CONSTRAINT storage_pkey PRIMARY KEY (collection, `key`, user_id),
    CONSTRAINT storage_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    INDEX `collection_read_key_user_id_idx` (collection, `read`, `key`, user_id),
    INDEX `collection_read_user_id_key_idx` (collection, `read`, user_id, `key`),
    INDEX `collection_user_id_read_key_idx` (collection, user_id, `read`, `key`),
    INDEX `storage_auto_index_fk_user_id_ref_users` (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `subscription` (
    original_transaction_id VARCHAR(512) NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    environment smallint DEFAULT 0 NOT NULL,
    product_id VARCHAR(512) NOT NULL,
    purchase_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    store smallint DEFAULT 0 NOT NULL,
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin DEFAULT '00000000-0000-0000-0000-000000000000' NOT NULL,
    expire_time DATETIME(6) NOT NULL,
    raw_response JSON DEFAULT ('{}') NOT NULL,
    raw_notification JSON DEFAULT ('{}') NOT NULL,
    refund_time DATETIME(6) DEFAULT '1970-01-01 00:00:00' NOT NULL,
    CONSTRAINT subscription_original_transaction_id_check CHECK ((CHAR_LENGTH((original_transaction_id)) > 0)),
    CONSTRAINT subscription_pkey PRIMARY KEY (original_transaction_id),
    CONSTRAINT subscription_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT,
    INDEX `subscription_time_user_id_transaction_id_idx` (purchase_time DESC, user_id DESC, original_transaction_id DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `user_device` (
    id VARCHAR(128) NOT NULL,
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    preferences JSON DEFAULT ('{}') NOT NULL,
    push_token_amazon VARCHAR(512) DEFAULT '' NOT NULL,
    push_token_android VARCHAR(512) DEFAULT '' NOT NULL,
    push_token_huawei VARCHAR(512) DEFAULT '' NOT NULL,
    push_token_ios VARCHAR(512) DEFAULT '' NOT NULL,
    push_token_web VARCHAR(512) DEFAULT '' NOT NULL,
    CONSTRAINT user_device_pkey PRIMARY KEY (id),
    CONSTRAINT user_device_user_id_id_key UNIQUE (user_id, id),
    CONSTRAINT user_device_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `user_edge` (
    source_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `position` bigint NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    destination_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state smallint DEFAULT 0 NOT NULL,
    metadata JSON DEFAULT ('{}') NOT NULL,
    CONSTRAINT user_edge_destination_id_check CHECK ((destination_id <> '00000000-0000-0000-0000-000000000000')),
    CONSTRAINT user_edge_source_id_check CHECK ((source_id <> '00000000-0000-0000-0000-000000000000')),
    CONSTRAINT user_edge_pkey PRIMARY KEY (source_id, state, `position`),
    CONSTRAINT user_edge_source_id_destination_id_key UNIQUE (source_id, destination_id),
    CONSTRAINT user_edge_destination_id_fkey FOREIGN KEY (destination_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT user_edge_source_id_fkey FOREIGN KEY (source_id) REFERENCES users(id) ON DELETE CASCADE,
    INDEX `user_edge_auto_index_fk_destination_id_ref_users` (destination_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `user_tombstone` (
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    CONSTRAINT user_tombstone_pkey PRIMARY KEY (create_time, user_id),
    CONSTRAINT user_tombstone_user_id_key UNIQUE (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `users_notes` (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    note LONGTEXT NOT NULL,
    create_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,
    update_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,
    CONSTRAINT users_notes_id_key UNIQUE (id),
    CONSTRAINT users_notes_pkey PRIMARY KEY (user_id, create_time, id),
    CONSTRAINT users_notes_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
CREATE TABLE `wallet_ledger` (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    changeset JSON NOT NULL,
    metadata JSON NOT NULL,
    create_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    update_time DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) NOT NULL,
    CONSTRAINT wallet_ledger_id_key UNIQUE (id),
    CONSTRAINT wallet_ledger_pkey PRIMARY KEY (user_id, create_time, id),
    CONSTRAINT wallet_ledger_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- statement-break
INSERT INTO users (id, username) VALUES ('00000000-0000-0000-0000-000000000000', '');
-- statement-break
INSERT INTO setting (name, value) VALUES ('utc_toggle', 'false');
-- statement-break
-- InnoDB has no ON DELETE SET DEFAULT. Preserve purchase ownership tombstones.
CREATE TRIGGER users_purchase_tombstone BEFORE DELETE ON users FOR EACH ROW
BEGIN
    UPDATE purchase SET user_id = '00000000-0000-0000-0000-000000000000' WHERE user_id = OLD.id;
    UPDATE subscription SET user_id = '00000000-0000-0000-0000-000000000000' WHERE user_id = OLD.id;
END;
