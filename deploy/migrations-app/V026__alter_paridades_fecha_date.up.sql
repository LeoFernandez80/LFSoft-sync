-- Migration V026 (up): "paridad_fecha" is a calendar day, not an instant.
-- Existing values are either UTC midnight (app form) or local midnight (legacy sync, UTC-03);
-- rounding to the nearest UTC day maps both to the intended date. Idempotent.
DO $$
BEGIN
    IF (SELECT data_type FROM information_schema.columns
        WHERE table_schema = 'articulos' AND table_name = 'paridades' AND column_name = 'paridad_fecha') <> 'date' THEN
        ALTER TABLE articulos."paridades"
            ALTER COLUMN "paridad_fecha" TYPE DATE
            USING ((("paridad_fecha" AT TIME ZONE 'UTC') + INTERVAL '12 hours')::date);
    END IF;
END $$;
