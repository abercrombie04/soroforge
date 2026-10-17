-- SoroForge deployment history, SQLite dialect.
--
-- Same two tables and the same constraints as the Postgres migration, written
-- for SQLite: integer autoincrement keys instead of identity columns, and
-- TIMESTAMP (parsed to time.Time by modernc.org/sqlite) instead of TIMESTAMPTZ.

CREATE TABLE IF NOT EXISTS contracts (
    id                INTEGER     PRIMARY KEY AUTOINCREMENT,
    alias             TEXT        NOT NULL,
    network           TEXT        NOT NULL,
    contract_id       TEXT        NOT NULL,
    current_wasm_hash TEXT        NOT NULL,
    created_at        TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- An alias identifies a contract only within a network: "counter" on
    -- testnet and "counter" on mainnet are independent deployments.
    CONSTRAINT contracts_network_alias_key UNIQUE (network, alias)
);

CREATE TABLE IF NOT EXISTS deployments (
    id              INTEGER     PRIMARY KEY AUTOINCREMENT,
    alias           TEXT        NOT NULL,
    network         TEXT        NOT NULL,
    contract_id     TEXT        NOT NULL,
    wasm_hash       TEXT        NOT NULL,
    action          TEXT        NOT NULL,
    deployer_pubkey TEXT        NOT NULL,
    tx_hash         TEXT,
    ledger          INTEGER,
    notes           TEXT,
    created_at      TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT deployments_action_check CHECK (action IN ('deploy', 'upgrade'))
);

-- Required by the spec and used by every history and drift-detection query.
CREATE INDEX IF NOT EXISTS contracts_network_contract_id_idx
    ON contracts (network, contract_id);

CREATE INDEX IF NOT EXISTS deployments_network_contract_id_idx
    ON deployments (network, contract_id);

-- `soroforge history <alias>` reads newest-first for one contract on one
-- network; this index serves that ordering directly.
CREATE INDEX IF NOT EXISTS deployments_network_alias_created_at_idx
    ON deployments (network, alias, created_at DESC);
