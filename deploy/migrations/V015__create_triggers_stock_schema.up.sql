-- Migration V015: triggers for stock inventories and stock movements on the schema that holds
-- the tables. LFSoft-app stock V001 moved them from articulos to their own schema "stock"
-- (LFSoft-app/RFC/RFC-Stock-Separacion.md); before that move they lived in articulos.
-- Each trigger is created only if its table exists, so the order between this and the
-- app migrations doesn't matter. Idempotent.
-- Requires sync.notify_change (V001/V006) and sync.notify_document_change (V010).
DO $$
DECLARE
    sch TEXT;
BEGIN
    sch := CASE
        WHEN to_regclass('stock."stockInventories"') IS NOT NULL THEN 'stock'
        WHEN to_regclass('articulos."stockInventories"') IS NOT NULL THEN 'articulos'
    END;
    IF sch IS NULL THEN
        RETURN;
    END IF;

    -- Stock inventories (whole document).
    EXECUTE format('DROP TRIGGER IF EXISTS trg_stockInventories_sync ON %I."stockInventories"', sch);
    EXECUTE format('CREATE TRIGGER trg_stockInventories_sync
        AFTER INSERT OR UPDATE OR DELETE ON %I."stockInventories"
        FOR EACH ROW EXECUTE FUNCTION sync.notify_change(''stockInventories'', ''stockInventory_id'')', sch);

    EXECUTE format('DROP TRIGGER IF EXISTS trg_stockInventoryLines_sync ON %I."stockInventoryLines"', sch);
    EXECUTE format('CREATE TRIGGER trg_stockInventoryLines_sync
        AFTER INSERT OR UPDATE OR DELETE ON %I."stockInventoryLines"
        FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change(''stockInventories'', ''stockInventoryLine_inventory_id'')', sch);

    -- Stock movements: only new movements travel (neither side edits a saved movement; the app
    -- annuls with a new reversal movement). The lines are inserted in the same transaction, so
    -- when the sync reads the queued header they are already there.
    EXECUTE format('DROP TRIGGER IF EXISTS trg_stockMovements_sync ON %I."stockMovements"', sch);
    EXECUTE format('CREATE TRIGGER trg_stockMovements_sync
        AFTER INSERT ON %I."stockMovements"
        FOR EACH ROW EXECUTE FUNCTION sync.notify_change(''stockMovements'', ''stockMovement_id'')', sch);
END $$;
