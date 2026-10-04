-- Migration V007: sync trigger for articulos.paridades. Idempotent.
DROP TRIGGER IF EXISTS trg_paridades_sync ON articulos.paridades;
CREATE TRIGGER trg_paridades_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos.paridades
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('paridades', 'paridad_id');
