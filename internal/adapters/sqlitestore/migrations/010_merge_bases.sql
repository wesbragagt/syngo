-- The merge bases: recent whole versions of a text file's content, kept so a
-- later pair of concurrent edits can be merged against the version they both
-- came from. A three-way merge needs that content, and nothing else on this
-- device holds it once the file has been edited over.
--
-- Rows are written only while the text_merge setting is on, only for files
-- that are text and below the merge size limit, and only the newest few per
-- path are kept, so a device that leaves the setting off carries an empty
-- table. They are deleted with the file row through the foreign key.
CREATE TABLE merge_bases (
  folder_id TEXT NOT NULL,
  rel_path  TEXT NOT NULL,
  version   TEXT NOT NULL,                       -- version vector, "ID:N,ID:N"
  hash      TEXT NOT NULL,                       -- 64 hex chars, BLAKE3-256 of the content
  content   BLOB NOT NULL,
  saved_at  INTEGER NOT NULL,                    -- Unix nanoseconds
  PRIMARY KEY (folder_id, rel_path, hash),
  FOREIGN KEY (folder_id, rel_path) REFERENCES files(folder_id, rel_path) ON DELETE CASCADE
);

-- The candidates of one path, newest first, which is the only way they are read.
CREATE INDEX merge_bases_path ON merge_bases(folder_id, rel_path, saved_at DESC);
