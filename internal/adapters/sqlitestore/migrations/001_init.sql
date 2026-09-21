CREATE TABLE schema_version (version INTEGER NOT NULL);

CREATE TABLE folders (
  id             TEXT PRIMARY KEY,               -- folder.ID, [a-z0-9._-]{1,64}
  path           TEXT NOT NULL UNIQUE,           -- absolute, cleaned
  label          TEXT NOT NULL DEFAULT '',
  mode           TEXT NOT NULL CHECK (mode IN ('sendreceive','sendonly','receiveonly')),
  delete_policy  TEXT NOT NULL CHECK (delete_policy IN ('propagate','never')),
  include_hidden INTEGER NOT NULL DEFAULT 0,
  symlink_policy TEXT NOT NULL CHECK (symlink_policy IN ('skip','store')),
  keep_empty_dirs INTEGER NOT NULL DEFAULT 1,
  paused         INTEGER NOT NULL DEFAULT 0,
  last_scan_at   INTEGER,                        -- unix ns, NULL = never
  last_error     TEXT,
  created_at     INTEGER NOT NULL
);

CREATE TABLE folder_ignore_patterns (
  folder_id TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  position  INTEGER NOT NULL,
  pattern   TEXT NOT NULL,
  PRIMARY KEY (folder_id, position)
);

CREATE TABLE files (
  folder_id    TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  rel_path     TEXT NOT NULL,                    -- slash-separated, relative to folder root
  kind         INTEGER NOT NULL,                 -- 1 file, 2 dir, 3 symlink
  size         INTEGER NOT NULL DEFAULT 0,
  mtime_ns     INTEGER NOT NULL DEFAULT 0,
  mode         INTEGER NOT NULL DEFAULT 0,       -- permission bits only
  hash         TEXT NOT NULL DEFAULT '',         -- 64 hex chars, '' for dirs/symlinks/deleted
  link_target  TEXT NOT NULL DEFAULT '',
  inode        INTEGER NOT NULL DEFAULT 0,
  deleted      INTEGER NOT NULL DEFAULT 0,       -- tombstone
  modified_by  TEXT NOT NULL DEFAULT '',         -- device.ID that produced this version
  sequence     INTEGER NOT NULL,                 -- per-folder monotonic, from folder_sequence
  synced_hash  TEXT NOT NULL DEFAULT '',         -- last hash known to match the peer (M6/M9)
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (folder_id, rel_path)
);
CREATE INDEX files_folder_hash ON files(folder_id, hash);
CREATE INDEX files_folder_seq  ON files(folder_id, sequence);

CREATE TABLE folder_sequence (folder_id TEXT PRIMARY KEY REFERENCES folders(id) ON DELETE CASCADE, next INTEGER NOT NULL);

CREATE TABLE changes (                            -- scan classification log, feeds status/"recent changes"
  seq       INTEGER PRIMARY KEY AUTOINCREMENT,
  folder_id TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  type      TEXT NOT NULL CHECK (type IN ('create','modify','delete','rename')),
  rel_path  TEXT NOT NULL,
  old_path  TEXT NOT NULL DEFAULT '',
  old_hash  TEXT NOT NULL DEFAULT '',
  new_hash  TEXT NOT NULL DEFAULT '',
  at        INTEGER NOT NULL
);

CREATE TABLE devices (
  id           TEXT PRIMARY KEY,                 -- device.ID (52-char base32)
  name         TEXT NOT NULL DEFAULT '',
  address      TEXT NOT NULL,                    -- host:port
  verified     INTEGER NOT NULL DEFAULT 0,
  added_at     INTEGER NOT NULL,
  last_seen_at INTEGER
);

CREATE TABLE transfers (                          -- inbound only; sender is stateless
  id          TEXT PRIMARY KEY,                  -- 32 hex chars
  folder_id   TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  rel_path    TEXT NOT NULL,
  peer_id     TEXT NOT NULL,
  hash        TEXT NOT NULL,                     -- expected whole-file hash
  size        INTEGER NOT NULL,
  mtime_ns    INTEGER NOT NULL,
  mode        INTEGER NOT NULL,
  chunk_size  INTEGER NOT NULL,
  chunk_count INTEGER NOT NULL,
  temp_path   TEXT NOT NULL,                     -- <root>/.syncthis/tmp/<id>.part
  state       TEXT NOT NULL CHECK (state IN ('pending','active','paused','verifying','done','failed')),
  error       TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX transfers_folder_path_live ON transfers(folder_id, rel_path) WHERE state NOT IN ('done','failed');

CREATE TABLE transfer_chunks (
  transfer_id TEXT NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,
  idx         INTEGER NOT NULL,
  hash        TEXT NOT NULL DEFAULT '',          -- chunk BLAKE3 once verified
  verified    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (transfer_id, idx)
);

CREATE TABLE conflicts (                          -- M9
  id            TEXT PRIMARY KEY,
  folder_id     TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  rel_path      TEXT NOT NULL,
  conflict_path TEXT NOT NULL,
  local_hash    TEXT NOT NULL,
  remote_hash   TEXT NOT NULL,
  peer_id       TEXT NOT NULL,
  detected_at   INTEGER NOT NULL,
  resolved      INTEGER NOT NULL DEFAULT 0
);
