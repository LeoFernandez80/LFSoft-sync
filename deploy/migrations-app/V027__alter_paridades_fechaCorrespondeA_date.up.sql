-- Migration V027 (up): "paridad_fechaCorrespondeA" is a calendar day, not an instant.
-- Same rounding as V026: UTC midnight (app) and local midnight (legacy sync, UTC-03) both map to the intended date. Idempotent.
DO $$
BEGIN
    IF (SELECT data_type FROM information_schema.columns
        WHERE table_schema = 'articulos' AND table_name = 'paridades' AND column_name = 'paridad_fechaCorrespondeA') <> 'date' THEN
        ALTER TABLE articulos."paridades"
            ALTER COLUMN "paridad_fechaCorrespondeA" TYPE DATE
            USING ((("paridad_fechaCorrespondeA" AT TIME ZONE 'UTC') + INTERVAL '12 hours')::date);
    END IF;
END $$;
