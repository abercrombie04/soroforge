package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runStoreSuite exercises the behaviour every Store implementation must have.
//
// It is written against the interface and run against MemoryStore here, and
// against Postgres in postgres_test.go when TEST_DATABASE_URL is set. Sharing
// the suite is what keeps the in-memory store — which every other package tests
// against — honest about matching real database semantics.
func runStoreSuite(t *testing.T, newStore func(t *testing.T) store.Store) {
	ctx := context.Background()

	t.Run("get unknown contract reports ErrNotFound", func(t *testing.T) {
		s := newStore(t)

		_, err := s.GetContract(ctx, "testnet", "absent")
		assert.True(t, errors.Is(err, store.ErrNotFound),
			"callers distinguish 'never deployed' from a real failure with errors.Is, got %v", err)
	})

	t.Run("upsert then get", func(t *testing.T) {
		s := newStore(t)

		saved, err := s.UpsertContract(ctx, store.Contract{
			Alias:           "counter",
			Network:         "testnet",
			ContractID:      "CCOUNTER",
			CurrentWasmHash: "aaaa",
		})
		require.NoError(t, err)
		assert.NotZero(t, saved.CreatedAt)

		got, err := s.GetContract(ctx, "testnet", "counter")
		require.NoError(t, err)
		assert.Equal(t, "CCOUNTER", got.ContractID)
		assert.Equal(t, "aaaa", got.CurrentWasmHash)
	})

	t.Run("upsert updates in place rather than duplicating", func(t *testing.T) {
		// An upgrade must move the current hash forward, not create a second
		// row for the same contract.
		s := newStore(t)

		_, err := s.UpsertContract(ctx, store.Contract{
			Alias: "counter", Network: "testnet", ContractID: "CCOUNTER", CurrentWasmHash: "aaaa",
		})
		require.NoError(t, err)

		_, err = s.UpsertContract(ctx, store.Contract{
			Alias: "counter", Network: "testnet", ContractID: "CCOUNTER", CurrentWasmHash: "bbbb",
		})
		require.NoError(t, err)

		got, err := s.GetContract(ctx, "testnet", "counter")
		require.NoError(t, err)
		assert.Equal(t, "bbbb", got.CurrentWasmHash)

		all, err := s.ListContracts(ctx, "testnet")
		require.NoError(t, err)
		assert.Len(t, all, 1)
	})

	t.Run("the same alias on two networks is two contracts", func(t *testing.T) {
		// This is the multi-network guarantee: an alias means nothing without a
		// network, and testnet state must never bleed into mainnet.
		s := newStore(t)

		_, err := s.UpsertContract(ctx, store.Contract{
			Alias: "counter", Network: "testnet", ContractID: "CTEST", CurrentWasmHash: "aaaa",
		})
		require.NoError(t, err)
		_, err = s.UpsertContract(ctx, store.Contract{
			Alias: "counter", Network: "mainnet", ContractID: "CMAIN", CurrentWasmHash: "bbbb",
		})
		require.NoError(t, err)

		testnet, err := s.GetContract(ctx, "testnet", "counter")
		require.NoError(t, err)
		mainnet, err := s.GetContract(ctx, "mainnet", "counter")
		require.NoError(t, err)

		assert.Equal(t, "CTEST", testnet.ContractID)
		assert.Equal(t, "CMAIN", mainnet.ContractID)
		assert.NotEqual(t, testnet.CurrentWasmHash, mainnet.CurrentWasmHash)
	})

	t.Run("list filters by network and empty lists all", func(t *testing.T) {
		s := newStore(t)

		for _, c := range []store.Contract{
			{Alias: "counter", Network: "testnet", ContractID: "C1", CurrentWasmHash: "a"},
			{Alias: "token", Network: "testnet", ContractID: "C2", CurrentWasmHash: "b"},
			{Alias: "counter", Network: "mainnet", ContractID: "C3", CurrentWasmHash: "c"},
		} {
			_, err := s.UpsertContract(ctx, c)
			require.NoError(t, err)
		}

		testnet, err := s.ListContracts(ctx, "testnet")
		require.NoError(t, err)
		assert.Len(t, testnet, 2)

		all, err := s.ListContracts(ctx, "")
		require.NoError(t, err)
		assert.Len(t, all, 3)

		// Ordered by network then alias, so output is stable across runs.
		assert.Equal(t, "mainnet", all[0].Network)
		assert.Equal(t, "counter", all[1].Alias)
		assert.Equal(t, "token", all[2].Alias)
	})

	t.Run("list of an empty store is empty, not nil", func(t *testing.T) {
		// The HTTP API encodes this directly; a nil slice would serialise as
		// null rather than [].
		s := newStore(t)

		got, err := s.ListContracts(ctx, "testnet")
		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("history records deploys and upgrades newest first", func(t *testing.T) {
		s := newStore(t)

		for _, d := range []store.Deployment{
			{
				Alias: "counter", Network: "testnet", ContractID: "C1", WasmHash: "v1",
				Action: store.ActionDeploy, DeployerPubkey: "GABC", TxHash: "tx1", Ledger: 100,
			},
			{
				Alias: "counter", Network: "testnet", ContractID: "C1", WasmHash: "v2",
				Action: store.ActionUpgrade, DeployerPubkey: "GABC", TxHash: "tx2", Ledger: 200,
				Notes: "fix rounding",
			},
		} {
			_, err := s.RecordDeployment(ctx, d)
			require.NoError(t, err)
		}

		history, err := s.History(ctx, "testnet", "counter")
		require.NoError(t, err)
		require.Len(t, history, 2)

		// Newest first: the upgrade must not appear to precede the deploy it
		// followed, even when both land in the same clock tick.
		assert.Equal(t, store.ActionUpgrade, history[0].Action)
		assert.Equal(t, "v2", history[0].WasmHash)
		assert.Equal(t, "fix rounding", history[0].Notes)
		assert.Equal(t, store.ActionDeploy, history[1].Action)
		assert.Equal(t, uint32(100), history[1].Ledger)
	})

	t.Run("history is scoped to one contract on one network", func(t *testing.T) {
		s := newStore(t)

		_, err := s.RecordDeployment(ctx, store.Deployment{
			Alias: "counter", Network: "testnet", ContractID: "C1", WasmHash: "a",
			Action: store.ActionDeploy, DeployerPubkey: "GABC",
		})
		require.NoError(t, err)
		_, err = s.RecordDeployment(ctx, store.Deployment{
			Alias: "counter", Network: "mainnet", ContractID: "C2", WasmHash: "b",
			Action: store.ActionDeploy, DeployerPubkey: "GABC",
		})
		require.NoError(t, err)
		_, err = s.RecordDeployment(ctx, store.Deployment{
			Alias: "token", Network: "testnet", ContractID: "C3", WasmHash: "c",
			Action: store.ActionDeploy, DeployerPubkey: "GABC",
		})
		require.NoError(t, err)

		history, err := s.History(ctx, "testnet", "counter")
		require.NoError(t, err)
		require.Len(t, history, 1)
		assert.Equal(t, "a", history[0].WasmHash)
	})

	t.Run("history of an unknown contract is empty, not an error", func(t *testing.T) {
		s := newStore(t)

		history, err := s.History(ctx, "testnet", "absent")
		require.NoError(t, err)
		assert.Empty(t, history)
	})

	t.Run("optional deployment fields survive a round trip when absent", func(t *testing.T) {
		// tx_hash, ledger, and notes are nullable; a dry run or a failed
		// submission leaves them unset and reading them back must not error.
		s := newStore(t)

		_, err := s.RecordDeployment(ctx, store.Deployment{
			Alias: "counter", Network: "testnet", ContractID: "C1", WasmHash: "a",
			Action: store.ActionDeploy, DeployerPubkey: "GABC",
		})
		require.NoError(t, err)

		history, err := s.History(ctx, "testnet", "counter")
		require.NoError(t, err)
		require.Len(t, history, 1)
		assert.Empty(t, history[0].TxHash)
		assert.Zero(t, history[0].Ledger)
		assert.Empty(t, history[0].Notes)
	})

	t.Run("an upload is recorded without a contract id", func(t *testing.T) {
		// Uploading installs bytecode, which has no contract address of its
		// own. The record must round-trip with the field empty and still appear
		// in the alias's history.
		s := newStore(t)

		_, err := s.RecordDeployment(ctx, store.Deployment{
			Alias: "counter", Network: "testnet", WasmHash: "c0ffee",
			Action: store.ActionUpload, DeployerPubkey: "GABC",
			TxHash: "tx-upload", Ledger: 300,
		})
		require.NoError(t, err)

		history, err := s.History(ctx, "testnet", "counter")
		require.NoError(t, err)
		require.Len(t, history, 1)
		assert.Equal(t, store.ActionUpload, history[0].Action)
		assert.Empty(t, history[0].ContractID)
		assert.Equal(t, "tx-upload", history[0].TxHash)
		assert.Equal(t, uint32(300), history[0].Ledger)
	})

	t.Run("invalid records are rejected", func(t *testing.T) {
		s := newStore(t)

		_, err := s.RecordDeployment(ctx, store.Deployment{
			Alias: "counter", Network: "testnet", ContractID: "C1", WasmHash: "a",
			Action: "sideways", DeployerPubkey: "GABC",
		})
		assert.ErrorContains(t, err, "action")

		_, err = s.UpsertContract(ctx, store.Contract{Alias: "counter"})
		assert.ErrorContains(t, err, "network")
	})
}

func TestMemoryStore(t *testing.T) {
	runStoreSuite(t, func(t *testing.T) store.Store {
		return store.NewMemoryStore()
	})
}

func TestMemoryStoreHistoryOrdersByIDWhenTimestampsTie(t *testing.T) {
	// A frozen clock is the worst case for ordering, and it is realistic: two
	// records written milliseconds apart can share a timestamp.
	s := store.NewMemoryStore()
	s.SetClock(func() time.Time { return time.Unix(1700000000, 0).UTC() })

	ctx := context.Background()
	for _, hash := range []string{"v1", "v2", "v3"} {
		action := store.ActionUpgrade
		if hash == "v1" {
			action = store.ActionDeploy
		}
		_, err := s.RecordDeployment(ctx, store.Deployment{
			Alias: "counter", Network: "testnet", ContractID: "C1", WasmHash: hash,
			Action: action, DeployerPubkey: "GABC",
		})
		require.NoError(t, err)
	}

	history, err := s.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 3)
	assert.Equal(t, []string{"v3", "v2", "v1"},
		[]string{history[0].WasmHash, history[1].WasmHash, history[2].WasmHash})
}

func TestSQLiteStore(t *testing.T) {
	runStoreSuite(t, func(t *testing.T) store.Store {
		// SQLite is pure Go and needs no database; this is the conformance proof
		// that the new store is substitutable for MemoryStore and Postgres.
		tmpDir := t.TempDir()
		path := filepath.Join(tmpDir, "soroforge.db")

		s, err := store.NewSQLite(context.Background(), path)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })

		// A migrated SQLite database is required by the suite because the schema
		// is Postgres-flavoured (identity columns, the action check constraint).
		migrator, err := store.NewMigrator(store.DSNForSQLite(path))
		require.NoError(t, err)
		require.NoError(t, migrator.Up())
		require.NoError(t, migrator.Close())

		return s
	})
}

func TestActionValid(t *testing.T) {
	assert.True(t, store.ActionDeploy.Valid())
	assert.True(t, store.ActionUpgrade.Valid())
	assert.True(t, store.ActionUpload.Valid())
	assert.False(t, store.Action("rollback").Valid())
	assert.False(t, store.Action("").Valid())
}

func TestDeploymentValidate(t *testing.T) {
	valid := store.Deployment{
		Alias: "counter", Network: "testnet", ContractID: "C1", WasmHash: "a",
		Action: store.ActionDeploy, DeployerPubkey: "GABC",
	}
	require.NoError(t, valid.Validate())

	tests := map[string]func(d *store.Deployment){
		"alias":           func(d *store.Deployment) { d.Alias = "" },
		"network":         func(d *store.Deployment) { d.Network = "" },
		"contract_id":     func(d *store.Deployment) { d.ContractID = "" },
		"wasm_hash":       func(d *store.Deployment) { d.WasmHash = "" },
		"deployer_pubkey": func(d *store.Deployment) { d.DeployerPubkey = "" },
	}
	for field, break_ := range tests {
		t.Run("missing "+field, func(t *testing.T) {
			d := valid
			break_(&d)
			assert.ErrorContains(t, d.Validate(), field)
		})
	}

	t.Run("an upload may omit contract_id", func(t *testing.T) {
		// Bytecode is content-addressed by its wasm hash; there is no contract
		// address to record until a deploy or upgrade names one.
		upload := valid
		upload.Action = store.ActionUpload
		upload.ContractID = ""
		assert.NoError(t, upload.Validate())
	})
}

func TestSQLiteStoreRejectsBadPath(t *testing.T) {
	_, err := store.NewSQLite(context.Background(), "")
	require.ErrorContains(t, err, "database path is required")
}

func TestSQLiteMigrationsAreReversible(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "migrations.db")
	url := store.DSNForSQLite(path)

	migrator, err := store.NewMigrator(url)
	require.NoError(t, err)
	defer func() { require.NoError(t, migrator.Close()) }()

	require.NoError(t, migrator.Up())

	version, dirty, err := migrator.Version()
	require.NoError(t, err)
	require.False(t, dirty, "migrations must not leave the database dirty")
	require.Equal(t, uint(2), version)

	truncateSQLiteDB(t, path)

	require.NoError(t, migrator.Down())
	require.NoError(t, migrator.Up())
}

func truncateSQLiteDB(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = db.ExecContext(ctx, "DELETE FROM deployments")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "DELETE FROM contracts")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "DELETE FROM sqlite_sequence WHERE name IN ('contracts', 'deployments')")
	require.NoError(t, err)
}

// The two dialects must stay at parity: the same migration versions, in the
// same order, so a schema change cannot land on one backend only.
func TestMigrationsStayAtParity(t *testing.T) {
	fs := store.MigrationsFS()

	names := func(dir string) []string {
		entries, err := fs.ReadDir(dir)
		require.NoError(t, err)
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}

	postgres := names("migrations/postgres")
	sqlite := names("migrations/sqlite")

	require.NotEmpty(t, postgres)
	assert.Equal(t, postgres, sqlite, "both dialects must ship the same migration files")
}

func TestSQLitePathFromDSN(t *testing.T) {
	assert.Equal(t, "/tmp/history.db", store.SQLitePath("sqlite:///tmp/history.db"))
	assert.Equal(t, "history.db", store.SQLitePath("sqlite://history.db"))
	assert.Equal(t, "sqlite:///tmp/history.db", store.DSNForSQLite("/tmp/history.db"))
}
