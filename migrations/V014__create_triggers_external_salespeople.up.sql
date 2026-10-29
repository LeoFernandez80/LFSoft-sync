-- Migration V014: triggers for the external salespeople catalog (LFSoft-app sales V031).
-- The special commissions queue their salesperson (document child, like precios.criterios
-- in V012). Idempotent.
-- Requires sync.notify_change (V001/V006) and sync.notify_document_change (V010).

DROP TRIGGER IF EXISTS trg_externalSalespeople_sync ON sales."externalSalespeople";
CREATE TRIGGER trg_externalSalespeople_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."externalSalespeople"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('externalSalespeople', 'externalSalesperson_id');

DROP TRIGGER IF EXISTS trg_externalSalespersonSpecialCommissions_sync ON sales."externalSalespersonSpecialCommissions";
CREATE TRIGGER trg_externalSalespersonSpecialCommissions_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."externalSalespersonSpecialCommissions"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('externalSalespeople', 'externalSalespersonSpecialCommission_externalSalesperson_id');
