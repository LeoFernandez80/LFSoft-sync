-- Migration V017: triggers for customerCategories and customerReferences on the schema that
-- holds each table. LFSoft-app sales V033 moved both from utilities to sales; before that they
-- lived in utilities (V012 created their triggers there). Each trigger is created only if its
-- table exists, so the order between this and the app migrations doesn't matter. Bases where
-- V012 already ran keep the triggers: they travel with the table on ALTER TABLE ... SET SCHEMA.
-- Trigger names are lowercased, like the unquoted ones V012 created (trg_customercategories_sync).
-- Idempotent. Requires sync.notify_change (V001/V006).
DO $$
DECLARE
    t   TEXT;
    pk  TEXT;
    sch TEXT;
BEGIN
    FOR t, pk IN VALUES ('customerCategories', 'customerCategory_id'),
                        ('customerReferences', 'customerReference_id') LOOP
        sch := CASE
            WHEN to_regclass(format('sales.%I', t)) IS NOT NULL THEN 'sales'
            WHEN to_regclass(format('utilities.%I', t)) IS NOT NULL THEN 'utilities'
        END;
        IF sch IS NOT NULL THEN
            EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I.%I', lower('trg_' || t || '_sync'), sch, t);
            EXECUTE format('CREATE TRIGGER %I
                AFTER INSERT OR UPDATE OR DELETE ON %I.%I
                FOR EACH ROW EXECUTE FUNCTION sync.notify_change(%L, %L)',
                lower('trg_' || t || '_sync'), sch, t, t, pk);
        END IF;
    END LOOP;
END $$;
