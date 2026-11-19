-- Migration V018: triggers for the expense reports (module "Gastos de viajes") on the schema
-- that holds each table. LFSoft-app gastos V001 moved expenseReports and its four line tables
-- from utilities to gastos (LFSoft-app/RFC/RFC-Gastos-Separacion.md); before that they lived
-- in utilities (V011 created their triggers there). Each trigger is created only if its table
-- exists, so the order between this and the app migrations doesn't matter. Bases where V011
-- already ran keep the triggers: they travel with the table on ALTER TABLE ... SET SCHEMA.
-- Trigger names are lowercased, like the unquoted ones V011 created (trg_expensereports_sync).
-- Idempotent. Requires sync.notify_change / sync.notify_document_change (V001/V006/V011).
DO $$
DECLARE
    t   TEXT;
    col TEXT;
    doc BOOLEAN;
    sch TEXT;
BEGIN
    FOR t, col, doc IN VALUES ('expenseReports',           'expenseReport_id',                         false),
                              ('expenseReportExpenses',    'expenseReportExpense_expenseReport_id',    true),
                              ('expenseReportTrips',       'expenseReportTrip_expenseReport_id',       true),
                              ('expenseReportWorkedHours', 'expenseReportWorkedHour_expenseReport_id', true),
                              ('expenseReportAdvances',    'expenseReportAdvance_expenseReport_id',    true) LOOP
        sch := CASE
            WHEN to_regclass(format('gastos.%I', t)) IS NOT NULL THEN 'gastos'
            WHEN to_regclass(format('utilities.%I', t)) IS NOT NULL THEN 'utilities'
        END;
        IF sch IS NOT NULL THEN
            EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I.%I', lower('trg_' || t || '_sync'), sch, t);
            EXECUTE format('CREATE TRIGGER %I
                AFTER INSERT OR UPDATE OR DELETE ON %I.%I
                FOR EACH ROW EXECUTE FUNCTION sync.%I(%L, %L)',
                lower('trg_' || t || '_sync'), sch, t,
                CASE WHEN doc THEN 'notify_document_change' ELSE 'notify_change' END,
                'expenseReports', col);
        END IF;
    END LOOP;
END $$;
