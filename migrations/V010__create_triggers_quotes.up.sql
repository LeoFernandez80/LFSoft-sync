-- Migration V010: sync triggers for presupuestos (presupuestos.yaml, direction both). Idempotent.
--
-- The PG -> Access worker writes whole documents, so a change to any line has to queue its
-- ROOT quote: sync.notify_document_change resolves the root id and queues an 'U' for it.
-- quoteItemWithdrawals has no trigger: that collection is owned by the legacy app
-- (direction access_to_pg) and is never written back.

CREATE OR REPLACE FUNCTION sync.notify_document_change()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
-- TG_ARGV[0]: entity; TG_ARGV[1]: this row's column holding its parent id;
-- TG_ARGV[2] (optional): query mapping that parent id ($1) to the root id.
DECLARE
    parent_id BIGINT;
    root_id   BIGINT;
BEGIN
    IF current_setting('sync.is_writing', true) = 'true' THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'DELETE' THEN
        EXECUTE format('SELECT ($1).%I', TG_ARGV[1]) USING OLD INTO parent_id;
    ELSE
        EXECUTE format('SELECT ($1).%I', TG_ARGV[1]) USING NEW INTO parent_id;
    END IF;
    root_id := parent_id;
    IF TG_NARGS > 2 AND parent_id IS NOT NULL THEN
        EXECUTE TG_ARGV[2] INTO root_id USING parent_id;
    END IF;
    -- Parent already gone (cascade delete): the root's own change covers the document.
    IF root_id IS NULL THEN
        RETURN NULL;
    END IF;
    INSERT INTO sync.pg_changes (ch_entity, ch_row_id, ch_operation)
    VALUES (TG_ARGV[0], root_id, 'U');
    PERFORM pg_notify('sync_change', TG_ARGV[0] || ':' || root_id);
    RETURN NULL;
END;
$function$;

DROP TRIGGER IF EXISTS trg_quotes_sync ON sales.quotes;
CREATE TRIGGER trg_quotes_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales.quotes
    FOR EACH ROW EXECUTE FUNCTION sync.notify_change('quotes', 'quote_id');

DROP TRIGGER IF EXISTS trg_quoteItems_sync ON sales."quoteItems";
CREATE TRIGGER trg_quoteItems_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."quoteItems"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('quotes', 'quoteItem_quote_id');

DROP TRIGGER IF EXISTS trg_quoteItemDetails_sync ON sales."quoteItemDetails";
CREATE TRIGGER trg_quoteItemDetails_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."quoteItemDetails"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('quotes', 'quoteItemDetail_quoteItem_id',
        'SELECT "quoteItem_quote_id" FROM sales."quoteItems" WHERE "quoteItem_id" = $1');

DROP TRIGGER IF EXISTS trg_quoteItemOthers_sync ON sales."quoteItemOthers";
CREATE TRIGGER trg_quoteItemOthers_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."quoteItemOthers"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('quotes', 'quoteItemOther_quoteItem_id',
        'SELECT "quoteItem_quote_id" FROM sales."quoteItems" WHERE "quoteItem_id" = $1');

DROP TRIGGER IF EXISTS trg_quoteItemRealDetails_sync ON sales."quoteItemRealDetails";
CREATE TRIGGER trg_quoteItemRealDetails_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."quoteItemRealDetails"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('quotes', 'quoteItemRealDetail_quoteItem_id',
        'SELECT "quoteItem_quote_id" FROM sales."quoteItems" WHERE "quoteItem_id" = $1');

DROP TRIGGER IF EXISTS trg_quoteItemRealOthers_sync ON sales."quoteItemRealOthers";
CREATE TRIGGER trg_quoteItemRealOthers_sync
    AFTER INSERT OR UPDATE OR DELETE ON sales."quoteItemRealOthers"
    FOR EACH ROW EXECUTE FUNCTION sync.notify_document_change('quotes', 'quoteItemRealOther_quoteItem_id',
        'SELECT "quoteItem_quote_id" FROM sales."quoteItems" WHERE "quoteItem_id" = $1');
