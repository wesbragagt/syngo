-- Folder-level device authorization. A device may read a folder's manifest and
-- its chunks, and learn the folder exists at all, only while it has a row
-- here. There is no row that means "everybody": an absent row denies, so a
-- folder nobody was granted is served to nobody.
CREATE TABLE folder_devices (
    folder_id  TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    device_id  TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    granted_at INTEGER NOT NULL,                  -- unix ns
    PRIMARY KEY (folder_id, device_id)
);
CREATE INDEX folder_devices_device ON folder_devices(device_id);

-- Every folder that exists when this migration runs is granted to every device
-- known at that moment. Before this schema a paired device could reach every
-- folder, so the backfill is what that deployment already had: an upgrade
-- changes no synchronization. Folders and devices added afterwards start with
-- no authorization and have to be granted.
INSERT INTO folder_devices (folder_id, device_id, granted_at)
SELECT f.id, d.id, CAST(strftime('%s', 'now') AS INTEGER) * 1000000000
FROM folders f CROSS JOIN devices d;
