-- The permission bits the local side last agreed on with a peer. 0 means
-- "never agreed on", which the sync plan treats as unknown rather than as a
-- local change.
ALTER TABLE files ADD COLUMN synced_mode INTEGER NOT NULL DEFAULT 0;
