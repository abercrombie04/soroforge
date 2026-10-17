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
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// SQLite is a pure-Go Store for single-user setups. It is backed by
// modernc.org/sqlite, so CGO_ENABLED=0 builds keep working.
//
// It is intended for a solo developer whose deploy history lives in a single
// file; Postgres remains the choice for teams and the HTTP API, whose writers
// and readiness checks assume a real database's concurrency semantics. See the
// README for when to choose which.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// NewSQLite opens (or creates) a SQLite file and returns a store around it.
//
// The path is a file path, e.g. /home/user/.soroforge/history.db, or a
// sqlite:/// absolute URI returned by a file:// URL's path. When the file does
// not exist it is created.
func NewSQLite(ctx context.Context, path string) (*SQLite, error) {
	if path == "" {
		return nil, fmt.Errorf("store: database path is required")
	}

	// modernc.org/sqlite is the pure-Go driver, registered under the "sqlite"
	// name so that sql.Open("sqlite", ...) opens a pure-Go database and
	// CGO_ENABLED=0 release builds keep working.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping sqlite: %w", err)
	}

	// WAL mode gives concurrent readers better throughput and avoids the
	// database being locked by a writer for long stretches.
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: enable WAL: %w", err)
	}

	return &SQLite{db: db}, nil
}

// SQLiteScheme is the DATABASE_URL prefix that selects the SQLite store.
//
// DATABASE_URL=sqlite:///home/me/.soroforge/history.db keeps a solo developer's
// history in one file; anything else is passed to Postgres.
const SQLiteScheme = "sqlite://"

// SQLitePath turns a sqlite:// DATABASE_URL into the file path the pure-Go
// driver expects. Everything after the scheme is the path, so sqlite:///x.db
// (an absolute path) and sqlite://x.db (relative) both work.
func SQLitePath(databaseURL string) string {
	return strings.TrimPrefix(databaseURL, SQLiteScheme)
}

// Close releases the database connection.
func (s *SQLite) Close() error { return s.db.Close() }

// DB exposes the underlying database for the migration path and for tests that
// build a schema directly.
func (s *SQLite) DB() *sql.DB { return s.db }

const sqliteUpsertContractSQL = `
INSERT INTO contracts (alias, network, contract_id, current_wasm_hash, created_at, updated_at)
VALUES ($1, $2, $3, $4, coalesce((SELECT created_at FROM contracts WHERE network = $2 AND alias = $1), CURRENT_TIMESTAMP), CURRENT_TIMESTAMP)
ON CONFLICT(network, alias) DO UPDATE
SET contract_id       = EXCLUDED.contract_id,
    current_wasm_hash = EXCLUDED.current_wasm_hash,
    updated_at        = CURRENT_TIMESTAMP
RETURNING id, alias, network, contract_id, current_wasm_hash, created_at, updated_at`

// UpsertContract implements Store.
func (s *SQLite) UpsertContract(ctx context.Context, c Contract) (Contract, error) {
	if err := c.Validate(); err != nil {
		return Contract{}, err
	}

	row := s.db.QueryRowContext(ctx, sqliteUpsertContractSQL, c.Alias, c.Network, c.ContractID, c.CurrentWasmHash)
	out, err := scanContract(row)
	if err != nil {
		return Contract{}, fmt.Errorf("upsert contract %s/%s: %w", c.Network, c.Alias, err)
	}
	return out, nil
}

const sqliteGetContractSQL = `
SELECT id, alias, network, contract_id, current_wasm_hash, created_at, updated_at
FROM contracts
WHERE network = $1 AND alias = $2`

// GetContract implements Store.
func (s *SQLite) GetContract(ctx context.Context, network, alias string) (Contract, error) {
	row := s.db.QueryRowContext(ctx, sqliteGetContractSQL, network, alias)
	c, err := scanContract(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Contract{}, fmt.Errorf("contract %s on %s: %w", alias, network, ErrNotFound)
	}
	if err != nil {
		return Contract{}, fmt.Errorf("get contract %s/%s: %w", network, alias, err)
	}
	return c, nil
}

const sqliteListContractsSQL = `
SELECT id, alias, network, contract_id, current_wasm_hash, created_at, updated_at
FROM contracts
ORDER BY network, alias`

// ListContracts implements Store.
//
// SQLite has no TIMESTAMPTZ and no empty-string shorthand for "all networks", so
// the filter is applied in Go to keep the two dialects at parity.
func (s *SQLite) ListContracts(ctx context.Context, network string) ([]Contract, error) {
	rows, err := s.db.QueryContext(ctx, sqliteListContractsSQL)
	if err != nil {
		return nil, fmt.Errorf("list contracts: %w", err)
	}
	defer rows.Close()

	contracts := []Contract{}
	for rows.Next() {
		c, err := scanContract(rows)
		if err != nil {
			return nil, fmt.Errorf("list contracts: %w", err)
		}
		if network == "" || c.Network == network {
			contracts = append(contracts, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list contracts: %w", err)
	}
	return contracts, nil
}

const sqliteRecordDeploymentSQL = `
INSERT INTO deployments (
    alias, network, contract_id, wasm_hash, action, deployer_pubkey, tx_hash, ledger, notes, created_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, coalesce((SELECT created_at FROM deployments WHERE id = -1), CURRENT_TIMESTAMP))
RETURNING id, alias, network, contract_id, wasm_hash, action,
          deployer_pubkey, tx_hash, ledger, notes, created_at`

// RecordDeployment implements Store.
func (s *SQLite) RecordDeployment(ctx context.Context, d Deployment) (Deployment, error) {
	if err := d.Validate(); err != nil {
		return Deployment{}, err
	}

	var txHash sql.NullString
	if d.TxHash != "" {
		txHash = sql.NullString{String: d.TxHash, Valid: true}
	}
	var ledger sql.NullInt64
	if d.Ledger != 0 {
		ledger = sql.NullInt64{Int64: int64(d.Ledger), Valid: true}
	}
	var notes sql.NullString
	if d.Notes != "" {
		notes = sql.NullString{String: d.Notes, Valid: true}
	}

	row := s.db.QueryRowContext(ctx, sqliteRecordDeploymentSQL,
		d.Alias, d.Network, d.ContractID, d.WasmHash, string(d.Action),
		d.DeployerPubkey, txHash, ledger, notes)

	out, err := scanDeployment(row)
	if err != nil {
		return Deployment{}, fmt.Errorf("record deployment for %s/%s: %w", d.Network, d.Alias, err)
	}
	return out, nil
}

const sqliteHistorySQL = `
SELECT id, alias, network, contract_id, wasm_hash, action,
       deployer_pubkey, tx_hash, ledger, notes, created_at
FROM deployments
WHERE network = $1 AND alias = $2
ORDER BY created_at DESC, id DESC`

// History implements Store.
//
// The ordering matches the Postgres query exactly: newest first, with ties
// broken by insertion order so an upgrade never appears to precede the deploy
// it followed.
func (s *SQLite) History(ctx context.Context, network, alias string) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, sqliteHistorySQL, network, alias)
	if err != nil {
		return nil, fmt.Errorf("history for %s/%s: %w", network, alias, err)
	}
	defer rows.Close()

	deployments := []Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("history for %s/%s: %w", network, alias, err)
		}
		deployments = append(deployments, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history for %s/%s: %w", network, alias, err)
	}
	return deployments, nil
}
