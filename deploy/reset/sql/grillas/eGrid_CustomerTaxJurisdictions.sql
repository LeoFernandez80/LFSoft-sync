-- Grilla eGrid_CustomerTaxJurisdictions (Jurisdicciones IIBB del cliente): solo el engranaje de configuracion; no cambia las columnas visibles.
-- Engranaje de configuracion de la grilla: la barra de generic-grid lo dibuja solo si el rol
-- tiene eAction_ConfigureGrid en la matriz de esta grilla (el motor es fail-closed). Se lo da a
-- ADMIN en todos los casos de la grilla, agregando la clave sin quitar las acciones que ya tengan.
DO $$
DECLARE
    grilla text := 'eGrid_CustomerTaxJurisdictions';
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
