-- Migration V002: triggers on synced tables
-- One trigger per table; add new triggers here as entities are enabled.
-- Each trigger calls sync.notify_change(entity_name, pk_column).
-- Idempotent: drops before create so re-runs don't fail.

-- articulos schema
DROP TRIGGER IF EXISTS trg_familias_sync ON articulos.familias;
CREATE TRIGGER trg_familias_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos.familias
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('familias', 'familia_id');

DROP TRIGGER IF EXISTS trg_grupos_sync ON articulos.grupos;
CREATE TRIGGER trg_grupos_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos.grupos
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('grupos', 'grupo_id');

DROP TRIGGER IF EXISTS trg_unidades_sync ON articulos."unidadesMedida";
CREATE TRIGGER trg_unidades_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos."unidadesMedida"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('unidades', 'unidadMedida_id');

DROP TRIGGER IF EXISTS trg_alicuotas_sync ON articulos."alicuotasIva";
CREATE TRIGGER trg_alicuotas_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos."alicuotasIva"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('alicuotas', 'alicuotaIva_id');

DROP TRIGGER IF EXISTS trg_articulos_sync ON articulos.articulos;
CREATE TRIGGER trg_articulos_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos.articulos
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('articles', 'articulo_id');
