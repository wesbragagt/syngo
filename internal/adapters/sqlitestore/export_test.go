package sqlitestore

import "fmt"

// MigrationSQL returns the script that brings the schema to the given version.
// Tests use it to build a database at an older version than the current one.
func MigrationSQL(version int) (string, error) {
	if version < 1 || version > len(migrationFiles) {
		return "", fmt.Errorf("no migration for version %d", version)
	}
	data, err := migrations.ReadFile(migrationFiles[version-1])
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// SchemaVersion is the version Open migrates a database to.
const SchemaVersion = schemaVersion
