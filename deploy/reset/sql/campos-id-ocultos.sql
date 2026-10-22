-- Campos ID ocultos en los filtros y forms de las entidades, para todos los roles.
-- Los filtros y forms dibujan el campo solo si no esta en seguridad.role_hidden_fields para el rol
-- y la pantalla (*ngIf="!isHiddenField('<entidad>_id')"). Este script agrega el <entidad>_id a la
-- lista de campos ocultos de cada rol en cada pantalla, sin quitar los que ya tenga. Es solo
-- presentacion: el ID sigue viajando en el JSON y el filtro por ID sigue funcionando por API.
-- Se puede correr mas de una vez. Con  -c lfsoft.revertir=on  (PGOPTIONS) quita el campo en vez
-- de agregarlo (Ocultar-CamposId.ps1 -Revertir).
DO $$
DECLARE
    pantallas text[][] := ARRAY[
        -- articulos
        ['eFilter_Actividad',           'actividad_id'],
        ['eForm_Actividad',             'actividad_id'],
        ['eFilter_AlicuotaIva',         'alicuotaIva_id'],
        ['eForm_AlicuotaIva',           'alicuotaIva_id'],
        ['eFilter_Articulo',            'articulo_id'],
        ['eForm_Articulo',              'articulo_id'],
        ['eFilter_Familia',             'familia_id'],
        ['eForm_Familia',               'familia_id'],
        ['eFilter_Grupo',               'grupo_id'],
        ['eForm_Grupo',                 'grupo_id'],
        ['eFilter_ImpuestoInterno',     'impuestoInterno_id'],
        ['eForm_ImpuestoInterno',       'impuestoInterno_id'],
        ['eFilter_Paridad',             'paridad_id'],
        ['eForm_Paridad',               'paridad_id'],
        ['eFilter_PosicionArancelaria', 'posicionArancelaria_id'],
        ['eForm_PosicionArancelaria',   'posicionArancelaria_id'],
        ['eFilter_UnidadMedida',        'unidadMedida_id'],
        ['eForm_UnidadMedida',          'unidadMedida_id'],
        -- precios
        ['eFilter_Criterio',            'criterio_id'],
        ['eForm_Criterio',              'criterio_id'],
        ['eFilter_ListaPrecios',        'listaPrecios_id'],
        ['eForm_ListaPrecios',          'listaPrecios_id'],
        ['eFilter_Moneda',              'moneda_id'],
        ['eForm_Moneda',                'moneda_id'],
        -- sales (los forms no muestran el ID; solo los filtros)
        ['eFilter_Customer',            'customer_id'],
        ['eFilter_Contact',             'contact_id'],
        ['eFilter_SubClient',           'subClient_id'],
        -- utilities
        ['eFilter_Country',             'country_id'],
        ['eForm_Country',               'country_id'],
        ['eFilter_Province',            'province_id'],
        ['eForm_Province',              'province_id'],
        ['eFilter_Locality',            'locality_id'],
        ['eForm_Locality',              'locality_id'],
        ['eFilter_Employee',            'employee_id'],
        ['eForm_Employee',              'employee_id']
    ];
    revertir  boolean := coalesce(current_setting('lfsoft.revertir', true), '') IN ('on', 'true', '1');
    i         int;
    pantalla  text;
    campo     text;
    rol       int;
    actual    text;
    activo    boolean;
    campos    text[];
    nuevo     text;
    cambios   int := 0;
BEGIN
    FOR rol IN SELECT "role_id" FROM seguridad."roles" ORDER BY "role_id" LOOP
        FOR i IN 1 .. array_length(pantallas, 1) LOOP
            pantalla := pantallas[i][1];
            campo    := pantallas[i][2];

            SELECT "role_hidden_field_hiddenFields", "role_hidden_field_isActive"
              INTO actual, activo
              FROM seguridad."role_hidden_fields"
             WHERE "role_hidden_field_userRole" = rol AND "role_hidden_field_literalKey" = pantalla;

            -- Lista limpia: el formato es '#|a|b|#', pero hay filas viejas 'a|b' o ''.
            campos := array_remove(array_remove(string_to_array(coalesce(actual, ''), '|'), '#'), '');

            IF revertir THEN
                CONTINUE WHEN actual IS NULL OR NOT (campo = ANY (campos));
                campos := array_remove(campos, campo);
            ELSE
                CONTINUE WHEN campo = ANY (campos);
                campos := campos || campo;
            END IF;

            nuevo := CASE WHEN cardinality(campos) = 0 THEN '#|#'
                          ELSE '#|' || array_to_string(campos, '|') || '|#' END;
            IF length(nuevo) > 255 THEN
                RAISE WARNING 'rol %, %: la lista de campos ocultos supera 255 caracteres, no se cambia', rol, pantalla;
                CONTINUE;
            END IF;

            IF actual IS NULL THEN
                INSERT INTO seguridad."role_hidden_fields" (
                    "role_hidden_field_userRole", "role_hidden_field_literalKey", "role_hidden_field_hiddenFields")
                VALUES (rol, pantalla, nuevo);
            ELSE
                UPDATE seguridad."role_hidden_fields"
                   SET "role_hidden_field_hiddenFields" = nuevo
                 WHERE "role_hidden_field_userRole" = rol AND "role_hidden_field_literalKey" = pantalla;
                IF NOT activo AND NOT revertir THEN
                    RAISE WARNING 'rol %, %: la fila de campos ocultos esta inactiva, el ID se va a seguir viendo hasta activarla', rol, pantalla;
                END IF;
            END IF;
            cambios := cambios + 1;
        END LOOP;
    END LOOP;

    RAISE NOTICE '% fila(s) de campos ocultos %', cambios, CASE WHEN revertir THEN 'sin el ID' ELSE 'con el ID' END;
END
$$;
