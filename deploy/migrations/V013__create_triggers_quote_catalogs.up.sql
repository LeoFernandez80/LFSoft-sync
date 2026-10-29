-- Migration V013: triggers for the quote catalogs added on 2026-10-27 (LFSoft-app sales V030,
-- utilities V025): delivery places, OT prints and accounting accounts. Idempotent.
-- Requires sync.notify_change (V001/V006). articulos.actividades already has its trigger
-- (V011); its new column actividad_accountingType only needs the entity mapping.

DROP TRIGGER IF EXISTS trg_deliveryPlaces_sync ON sales."deliveryPlaces";
CREATE TRIGGER trg_deliveryPlaces_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."deliveryPlaces"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('deliveryPlaces', 'deliveryPlace_id');

DROP TRIGGER IF EXISTS trg_quoteWorkOrderPrints_sync ON sales."quoteWorkOrderPrints";
CREATE TRIGGER trg_quoteWorkOrderPrints_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."quoteWorkOrderPrints"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('quoteWorkOrderPrints', 'quoteWorkOrderPrint_id');

DROP TRIGGER IF EXISTS trg_accountingAccounts_sync ON utilities."accountingAccounts";
CREATE TRIGGER trg_accountingAccounts_sync
    AFTER INSERT OR UPDATE OR DELETE ON utilities."accountingAccounts"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('accountingAccounts', 'accountingAccount_id');
