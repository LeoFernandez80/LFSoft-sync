-- Migration V008: keep sync triggers only for configured entities (articles, paridades).
-- Triggers without a worker fill sync.pg_changes with rows nobody processes. Idempotent;
-- skips tables that don't exist. Re-create a trigger here when its entity is enabled.
DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT * FROM (VALUES
        ('trg_familias_sync',   'articulos.familias'),
        ('trg_grupos_sync',     'articulos.grupos'),
        ('trg_unidades_sync',   'articulos."unidadesMedida"'),
        ('trg_alicuotas_sync',  'articulos."alicuotasIva"'),
        ('trg_impuestos_sync',  'articulos."impuestosInternos"'),
        ('trg_customers_sync',  'sales.customers'),
        ('trg_quotes_sync',     'sales.quotes')
    ) AS v(trg, tbl)
    LOOP
        IF to_regclass(t.tbl) IS NOT NULL THEN
            EXECUTE format('DROP TRIGGER IF EXISTS %I ON %s', t.trg, t.tbl);
        END IF;
    END LOOP;
END $$;
