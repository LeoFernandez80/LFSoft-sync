-- Migration V006: keep the deleted row's data so PG→Access deletes can resolve
-- business-key PKs (e.g. dates) without depending on sync.id_map.

ALTER TABLE sync.pg_changes ADD COLUMN IF NOT EXISTS ch_old_data JSONB;

CREATE OR REPLACE FUNCTION sync.notify_change()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
DECLARE
    row_id BIGINT;
    old_data JSONB;
BEGIN
    IF current_setting('sync.is_writing', true) = 'true' THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'DELETE' THEN
        EXECUTE format('SELECT ($1).%I', TG_ARGV[1]) USING OLD INTO row_id;
        old_data := to_jsonb(OLD);
    ELSE
        EXECUTE format('SELECT ($1).%I', TG_ARGV[1]) USING NEW INTO row_id;
    END IF;
    INSERT INTO sync.pg_changes (ch_entity, ch_row_id, ch_operation, ch_old_data)
    VALUES (TG_ARGV[0], row_id, LEFT(TG_OP, 1), old_data);
    PERFORM pg_notify('sync_change', TG_ARGV[0] || ':' || row_id);
    RETURN NULL;
END;
$function$;
