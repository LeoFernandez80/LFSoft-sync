-- Migration V029 (up): create sales."quoteItemWithdrawals" (legacy Presup.mdb -> Movimientos). Idempotent.
-- Copia de LFSoft-app/database/sales/migrations/V025__create_quoteItemWithdrawals.up.sql
--
-- POR QUE EXISTE
-- Movimientos guarda, por (Presupuesto_Numero, Item_Numero, Articulo_Codigo), la
-- cantidad NETA de mercaderia retirada de stock para el item: los retiros (52
-- Retiro Presup., 59 Retiro OF) suman y las devoluciones (2) restan
-- (PresupStock.bas, actualizarMovStockToDB). El legacy la usa para saber que se
-- retiro por item y para calcular la mercaderia reservada. El esquema sales no
-- tenia donde ponerla.
--
-- NO reemplaza a articulos."stockMovements": aquel es el libro de movimientos
-- (tabla Stock de Articulo.mdb); esta es el saldo retirado por item.
--
-- Clave logica del legacy: (Presupuesto, Item, Articulo) -> aca
-- UNIQUE (quoteItem_id, articleCode), que es tambien la clave de upsert del sync.
--
-- "quoteItemWithdrawal_article_id": cross-domain sin FK fisica (mismo criterio que
-- quoteItemDetail_article_id, sales V003). NULLABLE: el legacy acepta codigos que
-- no estan en el maestro; el codigo es el dato autoritativo.
SET client_encoding = 'UTF8';

CREATE TABLE IF NOT EXISTS sales."quoteItemWithdrawals" (
    "quoteItemWithdrawal_id"           INTEGER       NOT NULL GENERATED ALWAYS AS IDENTITY,
    "quoteItemWithdrawal_quoteItem_id" INTEGER       NOT NULL,
    "quoteItemWithdrawal_article_id"   INTEGER       NULL,
    "quoteItemWithdrawal_articleCode"  VARCHAR(50)   NOT NULL,
    "quoteItemWithdrawal_quantity"     NUMERIC(18,4) NOT NULL DEFAULT 0,
    "quoteItemWithdrawal_unitPrice"    NUMERIC(18,4) NOT NULL DEFAULT 0,
    -- accessControlled: visible solo para los roles habilitados.
    "quoteItemWithdrawal_unitCost"     NUMERIC(18,4) NOT NULL DEFAULT 0,
    "quoteItemWithdrawal_isActive"     BOOLEAN       NOT NULL DEFAULT TRUE,
    "quoteItemWithdrawal_createdAt"    TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT "PK_QuoteItemWithdrawals" PRIMARY KEY ("quoteItemWithdrawal_id"),
    CONSTRAINT "FK_QuoteItemWithdrawals_QuoteItems"
        FOREIGN KEY ("quoteItemWithdrawal_quoteItem_id")
        REFERENCES sales."quoteItems" ("quoteItem_id")
        ON DELETE CASCADE
        ON UPDATE NO ACTION,
    CONSTRAINT "UQ_QuoteItemWithdrawals_item_article"
        UNIQUE ("quoteItemWithdrawal_quoteItem_id", "quoteItemWithdrawal_articleCode")
);

CREATE INDEX IF NOT EXISTS "IX_QuoteItemWithdrawals_quoteItem_id" ON sales."quoteItemWithdrawals" ("quoteItemWithdrawal_quoteItem_id");
CREATE INDEX IF NOT EXISTS "IX_QuoteItemWithdrawals_article_id"   ON sales."quoteItemWithdrawals" ("quoteItemWithdrawal_article_id");
CREATE INDEX IF NOT EXISTS "IX_QuoteItemWithdrawals_articleCode"  ON sales."quoteItemWithdrawals" ("quoteItemWithdrawal_articleCode");

COMMENT ON TABLE  sales."quoteItemWithdrawals" IS 'Saldo neto de mercaderia retirada de stock por item de presupuesto y articulo. legacy Presup.mdb Movimientos. Entidad hija de quoteItems: se borra con el agregado.';
COMMENT ON COLUMN sales."quoteItemWithdrawals"."quoteItemWithdrawal_articleCode" IS 'legacy Articulo_Codigo (16 caracteres). Dato autoritativo de la linea.';
COMMENT ON COLUMN sales."quoteItemWithdrawals"."quoteItemWithdrawal_article_id"  IS 'Resolucion de articleCode contra articulos.articulos. Cross-domain sin FK fisica; NULL si el codigo no esta en el maestro.';
COMMENT ON COLUMN sales."quoteItemWithdrawals"."quoteItemWithdrawal_quantity"    IS 'legacy Articulo_Cantidad. Neto: retiros (52/59) menos devoluciones (2).';
COMMENT ON COLUMN sales."quoteItemWithdrawals"."quoteItemWithdrawal_unitPrice"   IS 'legacy Articulo_Precio (Articulo_Publico al momento del retiro).';
COMMENT ON COLUMN sales."quoteItemWithdrawals"."quoteItemWithdrawal_unitCost"    IS 'legacy Articulo_Costo. accessControlled.';
