ALTER TABLE `groups`
    DROP PRIMARY KEY,
    DROP INDEX groups_discovery_idx,
    ADD PRIMARY KEY (disable_time, lang_tag, edge_count, id);
