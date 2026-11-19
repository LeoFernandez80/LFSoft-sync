-- Migration V016: triggers for actividades and paridades on the schema that holds each table.
-- LFSoft-app sales V032 moved actividades from articulos to sales, and utilities V026 moved
-- paridades from articulos to utilities (LFSoft-app/RFC/RFC-Actividades-Paridades-Separacion.md);
-- before those moves both lived in articulos. Each trigger is created only if its table exists,
-- so the order between this and the app migrations doesn't matter. Idempotent.
-- Requires sync.notify_change (V001/V006).
DO $$
DECLARE
    sch TEXT;
BEGIN
    sch := CASE
        WHEN to_regclass('sales."actividades"') IS NOT NULL THEN 'sales'
        WHEN to_regclass('articulos."actividades"') IS NOT NULL THEN 'articulos'
    END;
    IF sch IS NOT NULL THEN
        EXECUTE format('DROP TRIGGER IF EXISTS trg_actividades_sync ON %I."actividades"', sch);
        EXECUTE format('CREATE TRIGGER trg_actividades_sync
            AFTER INSERT OR UPDATE OR DELETE ON %I."actividades"
            FOR EACH ROW EXECUTE FUNCTION sync.notify_change(''actividades'', ''actividad_id'')', sch);
    END IF;

    sch := CASE
        WHEN to_regclass('utilities."paridades"') IS NOT NULL THEN 'utilities'
        WHEN to_regclass('articulos."paridades"') IS NOT NULL THEN 'articulos'
    END;
    IF sch IS NOT NULL THEN
        EXECUTE format('DROP TRIGGER IF EXISTS trg_paridades_sync ON %I."paridades"', sch);
        EXECUTE format('CREATE TRIGGER trg_paridades_sync
            AFTER INSERT OR UPDATE OR DELETE ON %I."paridades"
            FOR EACH ROW EXECUTE FUNCTION sync.notify_change(''paridades'', ''paridad_id'')', sch);
    END IF;
END $$;
