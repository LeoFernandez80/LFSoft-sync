-- Migration V012: triggers for the entities and collections that became bidirectional
-- (2026-10-16): customer collections, geography and customer catalogs, price criteria,
-- employees, quote withdrawals, stock inventories and stock movements. Idempotent.
-- Requires sync.notify_change (V001/V006) and sync.notify_document_change (V010).

-- Customer collections queue their customer (the whole document is written back).
DROP TRIGGER IF EXISTS trg_subClients_sync ON sales."subClients";
CREATE TRIGGER trg_subClients_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."subClients"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('customers', 'subClient_customer_id');

DROP TRIGGER IF EXISTS trg_customerTaxJurisdictions_sync ON sales."customerTaxJurisdictions";
CREATE TRIGGER trg_customerTaxJurisdictions_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."customerTaxJurisdictions"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('customers', 'customerTaxJurisdiction_customer_id');

DROP TRIGGER IF EXISTS trg_contacts_sync ON sales.contacts;
CREATE TRIGGER trg_contacts_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales.contacts
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('customers', 'contact_customer_id');

DROP TRIGGER IF EXISTS trg_contactSchedules_sync ON sales."contactSchedules";
CREATE TRIGGER trg_contactSchedules_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."contactSchedules"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('customers', 'contactSchedule_contact_id',
        'SELECT "contact_customer_id" FROM sales.contacts WHERE "contact_id" = $1');

-- Localities: document rooted at the country (entity "localities", localidades.yaml).
DROP TRIGGER IF EXISTS trg_localities_sync ON utilities.localities;
CREATE TRIGGER trg_localities_sync
    AFTER INSERT OR UPDATE OR DELETE ON utilities.localities
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('localities', 'locality_country_id');

-- Customer catalogs.
DROP TRIGGER IF EXISTS trg_customerCategories_sync ON utilities."customerCategories";
CREATE TRIGGER trg_customerCategories_sync
    AFTER INSERT OR UPDATE OR DELETE ON utilities."customerCategories"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('customerCategories', 'customerCategory_id');

DROP TRIGGER IF EXISTS trg_customerReferences_sync ON utilities."customerReferences";
CREATE TRIGGER trg_customerReferences_sync
    AFTER INSERT OR UPDATE OR DELETE ON utilities."customerReferences"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('customerReferences', 'customerReference_id');

DROP TRIGGER IF EXISTS trg_iibbConditions_sync ON utilities."iibbConditions";
CREATE TRIGGER trg_iibbConditions_sync
    AFTER INSERT OR UPDATE OR DELETE ON utilities."iibbConditions"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('iibbConditions', 'iibbCondition_id');

DROP TRIGGER IF EXISTS trg_contactSections_sync ON utilities."contactSections";
CREATE TRIGGER trg_contactSections_sync
    AFTER INSERT OR UPDATE OR DELETE ON utilities."contactSections"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('contactSections', 'contactSection_id');

DROP TRIGGER IF EXISTS trg_banks_sync ON utilities.banks;
CREATE TRIGGER trg_banks_sync
    AFTER INSERT OR UPDATE OR DELETE ON utilities.banks
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('banks', 'bank_id');

-- Price criteria queue their list.
DROP TRIGGER IF EXISTS trg_criterios_sync ON precios.criterios;
CREATE TRIGGER trg_criterios_sync
    AFTER INSERT OR UPDATE OR DELETE ON precios.criterios
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('listasPrecios', 'criterio_listaPrecios_id');

-- Employees.
DROP TRIGGER IF EXISTS trg_employees_sync ON utilities.employees;
CREATE TRIGGER trg_employees_sync
    AFTER INSERT OR UPDATE OR DELETE ON utilities.employees
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('employees', 'employee_id');

-- Quote withdrawals queue their quote.
DROP TRIGGER IF EXISTS trg_quoteItemWithdrawals_sync ON sales."quoteItemWithdrawals";
CREATE TRIGGER trg_quoteItemWithdrawals_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."quoteItemWithdrawals"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('quotes', 'quoteItemWithdrawal_quoteItem_id',
        'SELECT "quoteItem_quote_id" FROM sales."quoteItems" WHERE "quoteItem_id" = $1');

-- Stock inventories (whole document).
DROP TRIGGER IF EXISTS trg_stockInventories_sync ON articulos."stockInventories";
CREATE TRIGGER trg_stockInventories_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos."stockInventories"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('stockInventories', 'stockInventory_id');

DROP TRIGGER IF EXISTS trg_stockInventoryLines_sync ON articulos."stockInventoryLines";
CREATE TRIGGER trg_stockInventoryLines_sync
    AFTER INSERT OR UPDATE OR DELETE ON articulos."stockInventoryLines"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('stockInventories', 'stockInventoryLine_inventory_id');

-- Stock movements: only new movements travel (neither side edits a saved movement; the app
-- annuls with a new reversal movement). The lines are inserted in the same transaction, so
-- when the sync reads the queued header they are already there.
DROP TRIGGER IF EXISTS trg_stockMovements_sync ON articulos."stockMovements";
CREATE TRIGGER trg_stockMovements_sync
    AFTER INSERT ON articulos."stockMovements"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('stockMovements', 'stockMovement_id');
