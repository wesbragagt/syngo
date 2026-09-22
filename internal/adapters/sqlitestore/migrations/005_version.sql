-- The version vector of the entry, "DEVICEID:N,DEVICEID:N" in device-id
-- order. Empty means the row predates vectors: it compares equal to every
-- other empty vector and below every non-empty one. Rows are not backfilled
-- from modified_by or sequence: sequences are per device and not comparable
-- across devices, so any guess could order a stale version above a newer one.
ALTER TABLE files ADD COLUMN version TEXT NOT NULL DEFAULT '';
