// Package store persists SoroForge's deployment history.
//
// The history is the point of the tool: it answers "which WASM hash is live on
// which network, and who put it there" long after the terminal that ran the
// deploy is gone. Two tables back that — contracts holds current state, and
// deployments is an append-only log of every deploy, upgrade, and upload.
//
// Everything is namespaced by network. A contract alias means nothing on its
// own; "counter on testnet" and "counter on mainnet" are separate records with
// separate lifecycles, and no query in this package crosses that boundary
// implicitly.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	// Registers the postgres driver that NewWithSourceInstance resolves from
	// the postgres:// scheme in DATABASE_URL.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"

	// Registers the sqlite scheme so the sqlite:// DSN that DSNForSQLite builds
	// resolves. This is the same modernc.org/sqlite driver SQLiteStore opens,
	// so migrations and the store agree on the file format without cgo.
	_ "github.com/golang-migrate/migrate/v4/database/sqlite"

	_ "modernc.org/sqlite"
)

// Migrations are embedded per dialect so a built binary carries its own schema
// and `soroforge migrate up` works from a single file with nothing to copy
// alongside it — which matters when the binary runs in a CI container.
//
// Two trees rather than one because the dialects disagree on DDL that matters
// here: Postgres uses identity columns and TIMESTAMPTZ, SQLite cannot, so a
// shared file would only run on one of them.
//
//go:embed migrations/postgres/*.sql
//go:embed migrations/sqlite/*.sql
var migrationsFS embed.FS

// Migrator applies schema migrations.
type Migrator struct {
	m *migrate.Migrate
}

// NewMigrator prepares migrations against the given database URL.
//
// It takes a URL rather than the pgx pool because golang-migrate needs its own
// database/sql handle; sharing the pool is not supported.
func NewMigrator(databaseURL string) (*Migrator, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("migrate: database URL is required")
	}

	// Register the SQLite driver once, before the first migrator is opened.
	// golang-migrate opens its own database/sql connection from the DSN; for
	// the sqlite:// scheme that connection needs a registered "sqlite" driver,
	// and this package's init alone is not dependable across toolchains.
	if isSQLiteURL(databaseURL) {
		ensureSQLiteDriverRegistered()
	}

	source, err := iofs.New(migrationsFS, migrationsDir(databaseURL))
	if err != nil {
		return nil, fmt.Errorf("migrate: load embedded migrations: %w", err)
	}

	mig, err := migrate.NewWithSourceInstance("iofs", source, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("migrate: open database: %w", err)
	}

	return &Migrator{m: mig}, nil
}

// isSQLiteURL reports whether databaseURL is a sqlite:// DSN that NewMigrator
// will hand to modernc.org/sqlite.
func isSQLiteURL(databaseURL string) bool {
	return len(databaseURL) > len(SQLiteScheme) && databaseURL[:len(SQLiteScheme)] == SQLiteScheme
}

// ensureSQLiteDriverRegistered exists only to pull modernc.org/sqlite into the
// link so that the sqlite:// DSN used by the migrator resolves at runtime
// without a forgotten-import error.
func ensureSQLiteDriverRegistered() {
	// modernc.org/sqlite registers itself with database/sql under "sqlite" via
	// its init. This function is called from NewMigrator when the DSN scheme is
	// sqlite://, so the driver is registered before golang-migrate asks
	// database/sql to open it.
	_, _ = sql.Open("sqlite", "")
}

// Up applies all pending migrations. Applying an already-current schema is not
// an error, so this is safe to run on every start.
func (m *Migrator) Up() error {
	if err := m.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Down reverts the most recent migration.
//
// It steps back one migration rather than tearing everything down, because
// `migrate down` on a database holding real deployment history should not be a
// single command away from dropping all of it.
func (m *Migrator) Down() error {
	if err := m.m.Steps(-1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// Version reports the current schema version and whether the last migration
// left the database dirty (failed partway and needs manual attention).
func (m *Migrator) Version() (version uint, dirty bool, err error) {
	version, dirty, err = m.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migrate version: %w", err)
	}
	return version, dirty, nil
}

// Close releases the migrator's database handles.
func (m *Migrator) Close() error {
	sourceErr, dbErr := m.m.Close()
	return errors.Join(sourceErr, dbErr)
}

// DSNForSQLite returns a DSN for the modernc.org/sqlite-backed store at the
// given file path. It is exported so that tests and the migrator can open the
// same file with the same pure-Go driver without passing internal types around.
func DSNForSQLite(path string) string {
	return SQLiteScheme + path
}

// migrationsDir names the embedded dialect directory for a DSN.
func migrationsDir(databaseURL string) string {
	if isSQLiteURL(databaseURL) {
		return "migrations/sqlite"
	}
	return "migrations/postgres"
}

// MigrationsFS exposes the embedded migrations, with a directory per dialect,
// for tests that check the two stay at parity or build a schema directly.
func MigrationsFS() embed.FS { return migrationsFS }
