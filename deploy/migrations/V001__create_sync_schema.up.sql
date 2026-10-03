-- Migration V001: sync schema + all metadata tables + trigger function
-- DBMS: PostgreSQL
SET client_encoding = 'UTF8';

CREATE SCHEMA IF NOT EXISTS sync;

-- id_map: vincula PK de Access con PK interno de PG.
-- Solo para entidades con legacy_code_column (clientes, presupuestos, etc.).
CREATE TABLE IF NOT EXISTS sync.id_map (
    id_map_id        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id_map_entity    VARCHAR(100) NOT NULL,
    id_map_legacyId  VARCHAR(50)  NOT NULL,
    id_map_pgId      BIGINT       NOT NULL,
    id_map_createdAt TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (id_map_entity, id_map_legacyId)
);
CREATE INDEX IF NOT EXISTS idx_id_map_entity_pg
    ON sync.id_map (id_map_entity, id_map_pgId);

-- sync_state: cursores por entidad
CREATE TABLE IF NOT EXISTS sync.sync_state (
    ss_entity          VARCHAR(100) PRIMARY KEY,
    ss_lastMaxPk       VARCHAR(50)  NULL,
    ss_lastTimestamp    TIMESTAMPTZ  NULL,
    ss_lastFileModTime TIMESTAMPTZ  NULL,
    ss_lastChangeId    BIGINT       NULL,
    ss_lastHashScan    TIMESTAMPTZ  NULL,
    ss_lastCount       BIGINT       NULL,
    ss_updatedAt       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- sync_state_rows: hash por fila para deteccion de cambios (capa 4 — hash)
CREATE TABLE IF NOT EXISTS sync.sync_state_rows (
    ssr_entity   VARCHAR(100) NOT NULL,
    ssr_legacyId VARCHAR(50)  NOT NULL,
    ssr_pgId     BIGINT       NULL,
    ssr_rowHash  BIGINT       NOT NULL,
    ssr_syncedAt TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (ssr_entity, ssr_legacyId)
);

-- sync_log: auditoria por ciclo de sync
CREATE TABLE IF NOT EXISTS sync.sync_log (
    sl_id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sl_entity      VARCHAR(100) NOT NULL,
    sl_direction   VARCHAR(15)  NOT NULL,
    sl_startedAt   TIMESTAMPTZ  NOT NULL,
    sl_finishedAt  TIMESTAMPTZ  NOT NULL,
    sl_strategy    VARCHAR(20)  NOT NULL,
    sl_detected    INT          NOT NULL,
    sl_skipped     INT          NOT NULL,
    sl_written     INT          NOT NULL,
    sl_conflicts   INT          NOT NULL DEFAULT 0,
    sl_errors      INT          NOT NULL DEFAULT 0,
    sl_errorDetail TEXT         NULL,
    sl_durationMs  INT          NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sync_log_entity_time
    ON sync.sync_log (sl_entity, sl_startedAt DESC);

-- pg_changes: cambios capturados por triggers para sync PG → Access
CREATE TABLE IF NOT EXISTS sync.pg_changes (
    ch_id        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ch_entity    VARCHAR(100) NOT NULL,
    ch_row_id    BIGINT       NOT NULL,
    ch_operation CHAR(1)      NOT NULL,  -- I/U/D
    ch_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    ch_processed BOOLEAN      NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS idx_pg_changes_pending
    ON sync.pg_changes (ch_entity, ch_id)
    WHERE NOT ch_processed;

-- id_conflicts: conflictos de ID detectados
CREATE TABLE IF NOT EXISTS sync.id_conflicts (
    ic_id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ic_entity        VARCHAR(100) NOT NULL,
    ic_pgId          BIGINT       NOT NULL,
    ic_originalId    BIGINT       NOT NULL,
    ic_newId         BIGINT       NULL,
    ic_status        VARCHAR(20)  NOT NULL DEFAULT 'pending',
    ic_detectedAt    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    ic_resolvedAt    TIMESTAMPTZ  NULL
);

-- Funcion trigger: captura cambios + NOTIFY para sync instantaneo.
-- Anti-loop: si sync.is_writing = 'true' (SET LOCAL por el sync service),
-- no registra el cambio — evita que Access→PG dispare PG→Access.
CREATE OR REPLACE FUNCTION sync.notify_change() RETURNS trigger AS $$
DECLARE
    row_id BIGINT;
BEGIN
    IF current_setting('sync.is_writing', true) = 'true' THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'DELETE' THEN
        EXECUTE format('SELECT ($1).%I', TG_ARGV[1]) USING OLD INTO row_id;
    ELSE
        EXECUTE format('SELECT ($1).%I', TG_ARGV[1]) USING NEW INTO row_id;
    END IF;
    INSERT INTO sync.pg_changes (ch_entity, ch_row_id, ch_operation)
    VALUES (TG_ARGV[0], row_id, LEFT(TG_OP, 1));
    PERFORM pg_notify('sync_change', TG_ARGV[0] || ':' || row_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
