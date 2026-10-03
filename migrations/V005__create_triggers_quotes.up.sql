-- Migration V005: trigger for quotes (sales schema)
-- Idempotent: drops before create so re-runs don't fail.

DROP TRIGGER IF EXISTS trg_quotes_sync ON sales.quotes;
CREATE TRIGGER trg_quotes_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales.quotes
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('quotes', 'quote_id');
