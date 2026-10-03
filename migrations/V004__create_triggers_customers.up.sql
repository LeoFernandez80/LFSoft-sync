-- Migration V004: trigger for customers (sales schema)
-- Idempotent: drops before create so re-runs don't fail.

DROP TRIGGER IF EXISTS trg_customers_sync ON sales.customers;
CREATE TRIGGER trg_customers_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales.customers
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('customers', 'customer_id');
