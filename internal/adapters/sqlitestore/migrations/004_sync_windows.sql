-- Sync windows in the device's local time, "HH:MM-HH:MM" separated by commas.
-- Empty means the folder is always open, which is what every row written
-- before this column existed means.
ALTER TABLE folders ADD COLUMN sync_windows TEXT NOT NULL DEFAULT '';
