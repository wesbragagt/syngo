-- Why a conflict was recorded: the sync plan reason, such as "both-modified",
-- "delete-vs-modify" or "replace-vs-modify". Rows written before this column
-- existed keep the empty string, which reads as "not recorded".
ALTER TABLE conflicts ADD COLUMN reason TEXT NOT NULL DEFAULT '';
