-- Migration V002: triggers on synced tables
-- One trigger per table; add new triggers here as entities are enabled.
-- Each trigger calls sync.notify_change(entity_name, pk_column).

-- articulos schema
CREATE TRIGGER trg_familias_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos.familias
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('familias', 'familia_id');

CREATE TRIGGER trg_grupos_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos.grupos
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('grupos', 'grupo_id');

CREATE TRIGGER trg_unidades_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos."unidadesMedida"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('unidades', 'unidadMedida_id');

CREATE TRIGGER trg_alicuotas_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos."alicuotasIva"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('alicuotas', 'alicuotaIva_id');

CREATE TRIGGER trg_articulos_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos.articulos
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('articles', 'articulo_id');
