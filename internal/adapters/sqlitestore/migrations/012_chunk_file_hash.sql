-- A chunk row now carries the whole-file hash of the content it was cut from.
--
-- Offsets and lengths alone cannot say which bytes they describe: an index left
-- behind by an earlier version of a file passes every geometry check whenever
-- the two versions are the same length, and the peer that asked for the list is
-- then sent a cut of content this device no longer holds. With the hash stored,
-- the server compares it against the file row before serving and re-chunks from
-- disk when they disagree.
--
-- Existing rows are stamped with the hash of the file row they belong to, which
-- is what they were written for: the index keeps working across the upgrade
-- instead of every path having to be read again. A row that was already stale
-- when the migration ran is blessed by it; the first scan or download of that
-- path rewrites it.
ALTER TABLE file_chunks ADD COLUMN file_hash TEXT NOT NULL DEFAULT '';

UPDATE file_chunks SET file_hash = COALESCE(
  (SELECT f.hash FROM files f
   WHERE f.folder_id = file_chunks.folder_id AND f.rel_path = file_chunks.rel_path), '');
