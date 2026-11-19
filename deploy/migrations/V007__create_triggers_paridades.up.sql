-- Migration V007: sync trigger for paridades. Idempotent.
-- The trigger now lives in V016, which creates it on whichever schema holds the table
-- (articulos in bases older than LFSoft-app utilities V026, utilities afterwards). Bases
-- where this migration already ran keep the trigger: it travels with the table on
-- ALTER TABLE ... SET SCHEMA.
SELECT 1;
