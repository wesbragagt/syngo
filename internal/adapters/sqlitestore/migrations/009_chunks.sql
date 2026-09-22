-- The chunk index: how every file of this device was cut, and the hash of each
-- piece. It is what lets a download take content this device already holds
-- instead of asking a peer for it, whatever file or folder that content is in.
--
-- The rows are written by the scanner, which is what reads a file's bytes, and
-- deleted with the file row through the foreign key. A database migrated from
-- version 7 starts with an empty table: nothing is re-downloaded for it, the
-- first scan after the upgrade fills it in, and until then a transfer simply
-- finds nothing to reuse.
CREATE TABLE file_chunks (
  folder_id TEXT NOT NULL,
  rel_path  TEXT NOT NULL,
  idx       INTEGER NOT NULL,
  start_offset INTEGER NOT NULL,
  byte_length  INTEGER NOT NULL,
  hash      TEXT NOT NULL,                       -- 64 hex chars, BLAKE3-256 of the chunk
  PRIMARY KEY (folder_id, rel_path, idx),
  FOREIGN KEY (folder_id, rel_path) REFERENCES files(folder_id, rel_path) ON DELETE CASCADE
) WITHOUT ROWID;

-- The deduplication lookup: given a chunk hash, where this device holds it.
CREATE INDEX file_chunks_hash ON file_chunks(hash);

-- Chunk boundaries are content-defined, so a transfer's chunks are no longer
-- derived from its size and chunk size: the sender says where it cut, and the
-- journal records it. A row written before this migration has NULL here and is
-- read back the old way, from the transfer's chunk size, so a download that was
-- in flight during the upgrade resumes instead of starting over.
ALTER TABLE transfer_chunks ADD COLUMN start_offset INTEGER;
ALTER TABLE transfer_chunks ADD COLUMN byte_length INTEGER;
