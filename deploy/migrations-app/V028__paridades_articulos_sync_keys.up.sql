-- Migration V028 (up): constraints required by the Access <-> PG sync. Idempotent.
-- - paridad_fecha and articulo_codigo are the business keys matched against Access.
-- - paridad_fechaCorrespondeA is optional in the legacy Access data.

ALTER TABLE articulos."paridades"
    ALTER COLUMN "paridad_fechaCorrespondeA" DROP NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'UQ_Paridades_fecha'
                   AND conrelid = 'articulos.paridades'::regclass) THEN
        ALTER TABLE articulos."paridades" ADD CONSTRAINT "UQ_Paridades_fecha" UNIQUE ("paridad_fecha");
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'UQ_Articulos_codigo'
                   AND conrelid = 'articulos.articulos'::regclass) THEN
        ALTER TABLE articulos."articulos" ADD CONSTRAINT "UQ_Articulos_codigo" UNIQUE ("articulo_codigo");
    END IF;
END $$;
