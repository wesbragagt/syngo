-- What the last sync cycle of a folder could not apply locally, and how many
-- items that was. The scanner owns last_error and the puller owns these two:
-- each clears only its own failures, so a folder whose root cannot be written
-- reports that even though its scans succeed.
ALTER TABLE folders ADD COLUMN last_sync_error TEXT NOT NULL DEFAULT '';
ALTER TABLE folders ADD COLUMN pending_items INTEGER NOT NULL DEFAULT 0;
