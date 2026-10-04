-- Migration V004: trigger for customers (sales schema)
-- Captures INSERT/UPDATE/DELETE on sales.customers for PG→Access sync.

CREATE TRIGGER trg_customers_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales.customers
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('customers', 'customer_id');
