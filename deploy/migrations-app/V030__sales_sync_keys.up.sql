-- Migration V030 (up): claves unicas que usa LFSoft-sync para emparejar filas con Access.
-- DBMS: PostgreSQL
-- Copia de LFSoft-app/database/sales/migrations/V026__add_sync_keys.up.sql
--
-- POR QUE EXISTE
-- El sync (LFSoft-sync, clientes.yaml) actualiza la fila PG que corresponde
-- a cada fila de Clientes.mdb buscandola por su clave legacy. Dos de esas claves no tenian
-- unicidad de base:
--
--   * ("contact_customer_id", "contact_legacyCode"). En Clientes.mdb la clave de Contactos
--     es (cliente, Id_Contacto), no Id_Contacto solo: el legacy copia contactos de un
--     cliente a otro conservando el Id_Contacto (38 casos). Unico PARCIAL, igual que
--     "UQ_Customers_legacyCode" (V014): los contactos creados en la app no tienen codigo.
--   * ("customerTaxJurisdiction_customer_id", "customerTaxJurisdiction_cmCode"). El legacy
--     ya lo controlaba antes de insertar (Modulo Clientes.bas); un cliente no repite
--     jurisdiccion.
--
-- Sin ellas, una fila duplicada haria que el sync actualice una cualquiera de las dos.
--
-- Idempotente.
SET client_encoding = 'UTF8';

CREATE UNIQUE INDEX IF NOT EXISTS "UQ_Contacts_customer_legacyCode"
    ON sales."contacts" ("contact_customer_id", "contact_legacyCode")
    WHERE "contact_legacyCode" IS NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'UQ_CustomerTaxJurisdictions_customer_cmCode'
                   AND conrelid = 'sales."customerTaxJurisdictions"'::regclass) THEN
        ALTER TABLE sales."customerTaxJurisdictions"
            ADD CONSTRAINT "UQ_CustomerTaxJurisdictions_customer_cmCode"
            UNIQUE ("customerTaxJurisdiction_customer_id", "customerTaxJurisdiction_cmCode");
    END IF;
END $$;
