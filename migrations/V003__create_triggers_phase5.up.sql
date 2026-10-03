-- Migration V003: triggers for Phase 5 entities
-- Idempotent: drops before create so re-runs don't fail.

DROP TRIGGER IF EXISTS trg_impuestos_sync ON articulos."impuestosInternos";
CREATE TRIGGER trg_impuestos_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos."impuestosInternos"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('impuestos', 'impuestoInterno_id');
