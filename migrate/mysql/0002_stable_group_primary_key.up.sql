-- Keep group identity stable while counts/language/disable state change under locks.
-- Preserve the former clustered key as a discovery index.
ALTER TABLE `groups`
    DROP PRIMARY KEY,
    ADD PRIMARY KEY (id),
    ADD UNIQUE INDEX groups_discovery_idx (disable_time, lang_tag, edge_count, id);
