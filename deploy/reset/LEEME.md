# LFSoft — paquete de reset de la base y sincronización completa con el legacy

Recrea desde cero la base `lfsoft_hidramic`, aplica las migraciones de estructura de la app (tablas, índices, vistas y configuración; **ninguna de datos del negocio**), instala el sync con **todas** las entidades de los módulos migrados, en los dos sentidos, y hace la carga inicial desde los `.mdb`. Al terminar, la app y el VB6 quedan trabajando sobre los mismos datos, y el servicio del sync los mantiene sincronizados.

> **Borra todo el contenido de la base.** Antes hace un backup completo (`pg_dump`) y se puede volver atrás con `Restore-LFSoft.ps1`. Se conservan los usuarios con rol ADMIN (con su contraseña) y los permisos y campos ocultos del rol ADMIN. El resto de los usuarios, roles y configuraciones de la app vuelve a los valores iniciales.

## Contenido

| Archivo | Qué es |
|---|---|
| `Reset-LFSoft.ps1` | El reset completo (10 pasos, ver abajo) |
| `Restore-LFSoft.ps1` | Vuelve la base y el sync al estado del backup |
| `Mantenimiento-Sync.ps1` | Mantenimiento nocturno (lo instala el reset como tarea programada) |
| `Aplicar-Grillas.ps1` | Aplica los scripts de `sql\grillas` (todos o algunos) sobre una base en uso |
| `Ocultar-CamposId.ps1` | Oculta los campos ID en filtros y forms (`sql\campos-id-ocultos.sql`) sobre una base en uso |
| `manifest-app.txt` | Orden en que se aplican las migraciones de la app |
| `migrations-app\<dominio>\` | Las migraciones de la app (articulos, precios, sales, seguridad, settings, utilities) |
| `sql\branding.sql` | Tabla `settings.branding`, solo si la base actual no la tiene |
| `sql\grillas\<grilla>.sql` | Un script por grilla que deja visibles solo sus columnas más relevantes y le da a ADMIN el engranaje de configuración |
| `sync\lfsoft-sync.exe` | Servicio del sync, Windows x64 |
| `sync\config\` | `sync.yaml` (solo se usa si no hay uno instalado) y las entidades |
| `sync\migrations\` | Esquema `sync` y triggers |

SHA256 de `sync\lfsoft-sync.exe`: `{{SHA256}}`

## Antes de empezar

- Avisar a los usuarios de la app: el backend queda detenido durante el reset (30 a 60 minutos).
- El VB6 puede seguir trabajando: el reset solo lee los `.mdb`.
- PowerShell **como Administrador**, parado en esta carpeta.

```powershell
# 0. Si Windows bloquea los scripts por venir de un zip
Get-ChildItem -Recurse | Unblock-File

# 1. Verificar sin tocar nada: conexión, nombre de la base, .mdb, backend, espacio para el backup
.\Reset-LFSoft.ps1 -SoloVerificar

# 2. Reset (pide escribir el nombre de la base para confirmar)
.\Reset-LFSoft.ps1
```

La conexión se toma del `sync.yaml` instalado (`C:\Apps\LFSoft\tools\sync\config\sync.yaml`); si hace falta otra: `-DatabaseUrl "postgres://postgres:<clave>@localhost:5432/lfsoft_hidramic"`. Si la conexión apunta a una base que no se llama `lfsoft_hidramic`, el script se detiene.

**Revisar en el paso 1** la advertencia de `search_path`: el `DATABASE_URL` del backend tiene que incluir `articulos,precios,sales,seguridad,settings,utilities,plataforma,public`. Si falta alguno, ese módulo da error 500.

## Qué hace `Reset-LFSoft.ps1`

1. **Verificaciones**: `psql`/`pg_dump`, conexión, nombre de la base, que estén todos los `.mdb`, servicio del backend y espacio libre.
2. **Confirmación**: escribir el nombre de la base (`-Forzar` la saltea).
3. **Detiene** el backend (servicio NSSM) y el sync.
4. **Backup** en `C:\Apps\LFSoft\backups\reset_<fecha>\`: `lfsoft_hidramic.dump` (verificado con `pg_restore -l`), los usuarios ADMIN con sus permisos (`admin.sql`), la tabla `settings.branding`, la instalación anterior del sync y una copia de los `.mdb` (`-SinBackupMdb` la omite).
5. **Borra todos los schemas** (incluido `public`) y crea vacíos `articulos, precios, sales, seguridad, settings, utilities, plataforma`.
6. **Migraciones de la app** en el orden de `manifest-app.txt`, registradas en `public.lfsoft_schema_migrations` (los próximos `Apply-Migrations.ps1` las ven como aplicadas). Las cinco migraciones de datos del negocio (actividades, paridades, catálogos de clientes, clientes y un remapeo de presupuestos) no vienen en el paquete y se registran como "omitida": esos datos los trae el sync desde Access. Sí se aplican las de configuración de la app (usuario administrador, permisos, literales, grillas) y los dos catálogos que el VB6 tiene fijos en su código y no en una tabla (monedas y tipos de movimiento de stock).
7. **Administrador, branding y grillas**: vuelve a cargar los usuarios ADMIN con su contraseña y los permisos del rol, y el nombre y logo de la empresa. Después corre `sql\grillas\*.sql`, que dejan visibles solo las columnas más relevantes de cada grilla (`-SinGrillas` lo omite).
8. **Instala el sync** en `C:\Apps\LFSoft\tools\sync`: exe, entidades (reemplaza todas las anteriores) y migraciones. El `sync.yaml` instalado se conserva.
9. **Carga inicial**: una pasada completa Access → PG de cada entidad, en orden de dependencias, con una segunda pasada para las que tuvieron errores. Muestra una tabla con filas leídas, escritas, errores y el conteo Access vs PG.
10. **Servicios**: crea y arranca `LFSoftSync`, registra la tarea **"LFSoft Sync - Mantenimiento"** (todos los días a las 03:30) y vuelve a arrancar el backend.

Todo queda en `logs\reset_<fecha>.log` dentro de esta carpeta.

## Qué se sincroniza

Las sincronizaciones periódicas las hace el servicio: cada entidad revisa sus cambios cada pocos segundos y hace un barrido completo cada cierto tiempo (`full_scan_interval` de cada YAML). La tarea nocturna reinicia el servicio, y el primer barrido después de arrancar compara todas las filas: esa es la reconciliación completa diaria. La tarea también rota `sync.log` y purga las tablas de control del sync.

Todas las entidades viajan **en los dos sentidos**:

| Módulo | Entidad (legacy ↔ PG) |
|---|---|
| Artículos | Familias, grupos, unidades de medida, alícuotas, impuestos internos (códigos por `/sync/next-id`), actividades, posiciones arancelarias, artículos, paridades |
| Stock | Movimientos de stock (ingresos, egresos, conversiones, ajustes, retiros), inventarios físicos con su detalle |
| Precios | Listas de precios con sus criterios |
| Clientes | Clientes con subclientes, jurisdicciones IIBB, contactos y horarios; países, provincias, localidades, categorías, referencias, condiciones IIBB, secciones de contacto, bancos |
| Presupuestos | Presupuestos con ítems, detalles, otros, reales y retiros de stock |
| Rendiciones | Planillas con gastos, tramos, horas y anticipos; empleados |

Lo que se da de alta en la app recibe el código que espera el VB6 antes de llegar a Access: clientes, contactos, países, provincias, catálogos y listas nuevas toman el máximo de Access + 1 (y ese código queda guardado en PG). Presupuestos, rendiciones, movimientos de stock, inventarios y empleados mantienen su número; si el VB6 llegara a usar el mismo número de un movimiento de stock antes de que llegue, el de la app se renumera.

La única excepción es **Fechas de terminación** de presupuestos (la primera factura de cada presupuesto, en Factura.mdb): no es una tabla del legacy sino un cálculo sobre las facturas, así que solo se lee.

**Stock**: en el VB6 cada fila de `Stock` es una línea con su propio tipo (una conversión son filas 6 y 57 con el mismo número) y ningún lado modifica un movimiento grabado (la app anula con un contra-asiento, que en Access entra como cantidades negativas). Por eso cada movimiento se copia entero una vez, en el sentido en que se creó. Los movimientos del legacy borrados en Access (al eliminar una factura de proveedor) se borran en PG. Los saldos por artículo de la app quedan iguales a los del VB6.

Tiempos esperables:

- **App → VB6**: segundos (trigger en PG + aviso inmediato al sync).
- **VB6 → app**: segundos a un minuto según la entidad. En los documentos (presupuestos, rendiciones, clientes, inventarios), un cambio solo en una línea, sin tocar la cabecera, lo toma el barrido periódico: 10 minutos en rendiciones e inventarios, 15 en clientes, 2 horas en presupuestos (el VB6 actualiza la cabecera de un presupuesto al guardarlo, así que en la práctica entra antes).
- Después de cada arranque del servicio (y de la tarea nocturna) cada entidad compara todas sus filas: durante esos primeros minutos el sync trabaja más y `/sync/next-id` puede tardar. Si tarda más de 2 segundos, la app numera con el máximo de PG y reintenta si el número ya existe.

## Qué queda solo en Access

Contra la copia de los `.mdb` del 29/09 (la tabla de la carga inicial, paso 9, muestra los números reales del servidor):

- **52 presupuestos de 2006–2015** y **5 inventarios**: usan artículos borrados del maestro de Access (y un presupuesto, un cliente que ya no existe). Sin artículo o cliente no se pueden guardar en PG. Si se vuelven a dar de alta esos artículos en el legacy (como discontinuados), el sync los trae solo.
- **1.535 movimientos de stock** (de 107.498) sin ninguna línea con un artículo existente: las filas "placeholder" sin artículo que graba la carga de facturas de proveedor y movimientos de códigos que ya no están en el maestro. No cambian ningún saldo de los artículos que existen. Las líneas sueltas de ese tipo dentro de otros movimientos también quedan afuera (1.795 de unas 295.000).
- **Inventario Final** (cierre de inventario por grupo y ejercicio): en la app son columnas del inventario que el legacy guarda en otra tabla; no se sincroniza.
- Usuarios de la app que no son ADMIN, roles y permisos de otros roles: no existen en el legacy.

## Después del reset

```powershell
Get-Content C:\Apps\LFSoft\tools\sync\logs\sync.log -Tail 50 -Wait
```

```sql
-- Último ciclo de cada entidad
SELECT DISTINCT ON (sl_entity) sl_entity, sl_direction, sl_finishedAt, sl_written, sl_errors, sl_errorDetail
FROM sync.sync_log ORDER BY sl_entity, sl_startedAt DESC;
```

- Entrar con el usuario ADMIN de siempre y dar de alta los demás usuarios y sus permisos.
- Probar en la app: abrir un artículo, un cliente, un presupuesto, una rendición y un movimiento de stock; crear uno de prueba y verificar que aparece en el VB6.

## Columnas visibles de las grillas

Cada grilla de la app tiene su script en `sql\grillas\` (por ejemplo `eGrid_Customers.sql`: razón social, CUIT y estado). El script cambia el valor por defecto del catálogo de columnas y también las configuraciones que los usuarios ya hayan guardado para esa grilla. Las columnas que quedan ocultas se pueden volver a mostrar desde la configuración de la grilla.

Cada script también le da al rol ADMIN el engranaje de configuración de la grilla (`eAction_ConfigureGrid` en `seguridad.role_permissions`): la grilla lo muestra solo si el rol tiene esa acción. La agrega sin quitar las acciones que el rol ya tenga. Las grillas que no cambian columnas (horarios de contacto, jurisdicciones IIBB, órdenes de trabajo de la rendición) tienen un script que solo agrega el engranaje.

El reset los aplica todos. Para aplicarlos sobre una base en uso (no hace falta detener el backend; la grilla toma el cambio la próxima vez que se abre el módulo) se puede usar este paquete o el paquete de grillas `LFSoft-grillas_<fecha>`, que trae solo `Aplicar-Grillas.ps1` y `sql\grillas` para llevar al servidor sin el resto:

```powershell
.\Aplicar-Grillas.ps1 -Listar                  # grillas y columnas que deja visibles
.\Aplicar-Grillas.ps1                          # todas
.\Aplicar-Grillas.ps1 -Grilla Customers, Quotes
```

Para cambiar qué columnas quedan visibles en una grilla, editar la lista `visibles` de su script (nombres de campo de `settings.grid_column_definitions`). Si un nombre no está en el catálogo, el script lo avisa y sigue.

## Campos ID ocultos en filtros y forms

`sql\campos-id-ocultos.sql` agrega el campo ID de cada entidad (`articulo_id`, `customer_id`, ...) a los campos ocultos de **todos los roles** en los filtros (`eFilter_*`) y forms (`eForm_*`) que lo muestran (`seguridad.role_hidden_fields`), sin quitar los campos que cada rol ya tenga ocultos. Es solo presentación: el ID sigue viajando en el JSON. El reset lo aplica en el paso 7, después de restaurar los campos ocultos de ADMIN.

Para aplicarlo sobre una base en uso (no hace falta detener el backend; los usuarios lo ven al recargar la app):

```powershell
.\Ocultar-CamposId.ps1             # oculta los ID
.\Ocultar-CamposId.ps1 -Revertir   # los vuelve a mostrar
```

Si una pantalla nueva muestra el ID, agregar el par (pantalla, campo) a la lista `pantallas` del script SQL.

## Volver atrás

```powershell
.\Restore-LFSoft.ps1 -Backup C:\Apps\LFSoft\backups\reset_<fecha>
```

Restaura la base desde el `pg_dump` y la instalación anterior del sync, y vuelve a arrancar los servicios. Los `.mdb` no se tocan salvo que se agregue `-RestaurarMdb`.
