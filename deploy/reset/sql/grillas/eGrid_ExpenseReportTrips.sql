-- Grilla eGrid_ExpenseReportTrips (Rendiciones: tramos): deja visibles solo: Salida, origen y destino.
-- Las demas columnas quedan ocultas; cada usuario las puede volver a mostrar desde la
-- configuracion de la grilla. Cambia el visible del catalogo (lo que ve quien nunca configuro
-- la grilla) y el de las configuraciones ya guardadas de esta grilla. Se puede correr mas de una vez.
DO $$
DECLARE
    grilla   text   := 'eGrid_ExpenseReportTrips';
    visibles text[] := ARRAY['expenseReportTrip_departureDate', 'expenseReportTrip_origin', 'expenseReportTrip_destination'];
    f        text;
BEGIN
    FOREACH f IN ARRAY visibles LOOP
        IF NOT EXISTS (SELECT 1 FROM settings."grid_column_definitions"
                        WHERE "grid_column_definition_gridLiteralKey" = grilla AND "grid_column_definition_field" = f) THEN
            RAISE WARNING '%: la columna % no esta en el catalogo', grilla, f;
        END IF;
    END LOOP;

    UPDATE settings."grid_column_definitions"
       SET "grid_column_definition_visible" = ("grid_column_definition_field" = ANY (visibles))
     WHERE "grid_column_definition_gridLiteralKey" = grilla;

    UPDATE settings."grid_column_configurations" gcc
       SET "grid_column_configuration_visible" = gcd."grid_column_definition_visible"
      FROM settings."grid_column_definitions" gcd
     WHERE gcd."grid_column_definition_id" = gcc."grid_column_configuration_gridColumnDefinitionId"
       AND gcd."grid_column_definition_gridLiteralKey" = grilla;
END
$$;

-- Engranaje de configuracion de la grilla: la barra de generic-grid lo dibuja solo si el rol
-- tiene eAction_ConfigureGrid en la matriz de esta grilla (el motor es fail-closed). Se lo da a
-- ADMIN en todos los casos de la grilla, agregando la clave sin quitar las acciones que ya tengan.
DO $$
DECLARE
    grilla text := 'eGrid_ExpenseReportTrips';
BEGIN
    INSERT INTO seguridad."permission_actions" ("permission_action_key", "permission_action_description")
    VALUES ('eAction_ConfigureGrid', 'Configure Grid')
    ON CONFLICT ("permission_action_key") DO NOTHING;

    INSERT INTO seguridad."literal_conditions" ("literal_condition_literalKey", "literal_condition_key", "literal_condition_description")
    VALUES (grilla, 'V', 'Caso general')
    ON CONFLICT ("literal_condition_literalKey", "literal_condition_key") DO NOTHING;

    IF NOT EXISTS (SELECT 1 FROM seguridad."role_permissions"
                    WHERE "role_permission_userRole" = 1 AND "role_permission_literalKey" = grilla) THEN
        INSERT INTO seguridad."role_permissions" (
            "role_permission_userRole", "role_permission_literalKey",
            "role_permission_caseId", "role_permission_conditions", "role_permission_actions")
        VALUES (1, grilla, 1, '#|V|#', '#|eAction_ConfigureGrid|#');
    END IF;

    UPDATE seguridad."role_permissions"
       SET "role_permission_actions" = replace("role_permission_actions", '|#', '|eAction_ConfigureGrid|#')
     WHERE "role_permission_userRole" = 1
       AND "role_permission_literalKey" = grilla
       AND "role_permission_actions" NOT LIKE '%|eAction_ConfigureGrid|%';
END
$$;
