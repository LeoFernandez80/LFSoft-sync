-- settings.branding: nombre y logo de la empresa que las SPAs piden en el arranque (/api/branding).
-- La creaba consolidacion\sql\93_branding.sql (Fase 1), que no es una migracion versionada:
-- el reset la crea vacia solo si la base anterior no la tenia (si la tenia, la restaura del
-- backup con sus datos). Fila unica: branding_id = 1. Sin fila, la app usa el logo generico.
CREATE TABLE IF NOT EXISTS settings."branding" (
    "branding_id"        INTEGER      NOT NULL DEFAULT 1,
    "branding_nombre"    VARCHAR(150) NOT NULL,
    "branding_logo"      BYTEA        NULL,
    "branding_logo_mime" VARCHAR(100) NULL,
    "branding_updatedAt" TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT "PK_Branding" PRIMARY KEY ("branding_id"),
    CONSTRAINT "CK_Branding_fila_unica" CHECK ("branding_id" = 1)
);
