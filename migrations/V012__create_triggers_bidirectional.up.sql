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
-- customerCategories, customerReferences: their triggers live in V017, which creates them on
-- whichever schema holds the tables (utilities in bases older than LFSoft-app sales V033,
-- sales afterwards). Bases where this migration already ran keep the triggers: they travel
-- with the table on ALTER TABLE ... SET SCHEMA.

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

-- Stock inventories, stock movements: their triggers live in V015, which creates them on
-- whichever schema holds the tables (articulos in bases older than LFSoft-app stock V001,
-- stock afterwards). Bases where this migration already ran keep the triggers: they
-- travel with the table on ALTER TABLE ... SET SCHEMA.
