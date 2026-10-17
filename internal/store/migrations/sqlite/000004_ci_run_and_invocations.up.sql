-- Mirrors postgres/000004_ci_run_and_invocations.up.sql: adds ci_run_url,
-- fn_name, and fn_args, and widens the action constraint to allow 'invoke'.
--
-- SQLite cannot alter a CHECK constraint in place, so the table is rebuilt
-- around the existing rows, as in 000002.

CREATE TABLE deployments_new (
    id              INTEGER     PRIMARY KEY AUTOINCREMENT,
    alias           TEXT        NOT NULL,
    network         TEXT        NOT NULL,
    contract_id     TEXT        NOT NULL,
    wasm_hash       TEXT        NOT NULL,
    action          TEXT        NOT NULL,
    deployer_pubkey TEXT        NOT NULL,
    tx_hash         TEXT,
    ledger          INTEGER,
    git_commit      TEXT,
    dirty           BOOLEAN,
    fee_charged     INTEGER,
    notes           TEXT,
    ci_run_url      TEXT,
    fn_name         TEXT,
    fn_args         TEXT,
    created_at      TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT deployments_action_check CHECK (action IN ('deploy', 'upgrade', 'upload', 'invoke'))
);

INSERT INTO deployments_new (
    id, alias, network, contract_id, wasm_hash, action, deployer_pubkey,
    tx_hash, ledger, git_commit, dirty, fee_charged, notes, created_at
)
SELECT id, alias, network, contract_id, wasm_hash, action, deployer_pubkey,
       tx_hash, ledger, git_commit, dirty, fee_charged, notes, created_at
FROM deployments;

DROP TABLE deployments;

ALTER TABLE deployments_new RENAME TO deployments;

-- Dropping the table dropped its indexes; recreate them for the new one.
CREATE INDEX IF NOT EXISTS deployments_network_contract_id_idx
    ON deployments (network, contract_id);

CREATE INDEX IF NOT EXISTS deployments_network_alias_created_at_idx
    ON deployments (network, alias, created_at DESC);
