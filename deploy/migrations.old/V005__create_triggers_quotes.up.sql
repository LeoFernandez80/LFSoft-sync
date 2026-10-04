CREATE TRIGGER trg_quotes_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales.quotes
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('quotes', 'quote_id');
