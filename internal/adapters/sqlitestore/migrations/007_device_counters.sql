-- The highest version counter ever seen for a device, this one included. It is
-- the floor every new version this device writes is raised to, so a database
-- restored from a backup does not hand out counters its peers already hold and
-- an edit made after the restore is not dominated by an older version of the
-- same path.
CREATE TABLE device_counters (
    device_id TEXT PRIMARY KEY,
    floor     INTEGER NOT NULL
);
