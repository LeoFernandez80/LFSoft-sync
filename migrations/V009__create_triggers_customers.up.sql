-- Migration V009: sync trigger for sales.customers (clientes.yaml, direction both). Idempotent.
-- Only the header table: subClients, customerTaxJurisdictions and contacts sync Access -> PG only.
DROP TRIGGER IF EXISTS trg_customers_sync ON sales.customers;
CREATE TRIGGER trg_customers_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales.customers
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('customers', 'customer_id');
