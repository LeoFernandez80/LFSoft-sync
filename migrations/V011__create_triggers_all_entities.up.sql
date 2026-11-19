-- Migration V011: sync triggers for every entity configured with direction both (paquete de
-- reset 2026-10-16). Idempotent: drops before create. Runs after V008, so it restores the
-- article catalog triggers V008 dropped while those entities were disabled.
--
-- Entities with collections that travel only Access -> PG (localities, criterios, stock
-- withdrawals) get no trigger on those tables: they are never written back.
-- Requires sync.notify_change (V001/V006) and sync.notify_document_change (V010).

-- Article catalogs (PK directa, codigos por /sync/next-id).
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

DROP TRIGGER IF EXISTS trg_impuestos_sync ON articulos."impuestosInternos";
CREATE TRIGGER trg_impuestos_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos."impuestosInternos"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('impuestos', 'impuestoInterno_id');

-- Actividades: its trigger lives in V016, created on whichever schema holds the table
-- (articulos before LFSoft-app sales V032, sales afterwards).

DROP TRIGGER IF EXISTS trg_posicionesArancelarias_sync ON articulos."posicionesArancelarias";
CREATE TRIGGER trg_posicionesArancelarias_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos."posicionesArancelarias"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('posicionesArancelarias', 'posicionArancelaria_id');

-- Price lists (criterios travels Access -> PG only).
DROP TRIGGER IF EXISTS trg_listasPrecios_sync ON precios."listasPrecios";
CREATE TRIGGER trg_listasPrecios_sync
    AFTER INSERT OR UPDATE OR DELETE ON precios."listasPrecios"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('listasPrecios', 'listaPrecios_id');

-- Geography (localities travels Access -> PG only).
DROP TRIGGER IF EXISTS trg_countries_sync ON utilities.countries;
CREATE TRIGGER trg_countries_sync
    AFTER INSERT OR UPDATE OR DELETE ON utilities.countries
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('countries', 'country_id');

DROP TRIGGER IF EXISTS trg_provinces_sync ON utilities.provinces;
CREATE TRIGGER trg_provinces_sync
    AFTER INSERT OR UPDATE OR DELETE ON utilities.provinces
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('provinces', 'province_id');

-- Expense reports (module "Gastos de viajes"): their triggers live in V018, created on whichever
-- schema holds the tables (utilities before LFSoft-app gastos V001, gastos afterwards).
