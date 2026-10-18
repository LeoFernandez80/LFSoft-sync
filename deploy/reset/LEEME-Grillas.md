# LFSoft — paquete de configuración de grillas

Aplica sobre una base en uso la configuración de las grillas de la app, **sin tocar nada más**: no hace backup ni reset, no aplica migraciones ni toca el sync, y no hace falta detener el backend. Cada grilla toma el cambio la próxima vez que se abre su módulo. Se puede correr más de una vez.

Por cada grilla:

- **Columnas visibles**: deja visibles solo las columnas más relevantes (por ejemplo, Clientes: razón social, CUIT y estado). Cambia el valor por defecto del catálogo de columnas y también las configuraciones que los usuarios ya hayan guardado para esa grilla. Las columnas que quedan ocultas se pueden volver a mostrar desde la configuración de la grilla.
- **Engranaje de configuración**: le da al rol ADMIN la acción `eAction_ConfigureGrid`, que es la que hace aparecer el engranaje en la barra de la grilla. La agrega sin quitar las acciones que el rol ya tenga. Algunas grillas (horarios de contacto, jurisdicciones IIBB, órdenes de trabajo de la rendición) solo reciben el engranaje.

## Contenido

| Archivo | Qué es |
|---|---|
| `Aplicar-Grillas.ps1` | Aplica los scripts de `sql\grillas` (todos o algunos) |
| `sql\grillas\<grilla>.sql` | Un script por grilla: columnas visibles y engranaje de configuración |

## Requisitos

- `psql.exe`: lo busca en `C:\Apps\LFSoft\tools\postgresql\bin`, en `C:\Program Files\PostgreSQL\*\bin` y en el PATH. Si no está en ninguno, pasar la carpeta con `-PgBin`.
- Conexión a la base: por defecto usa el `pg_dsn` del sync instalado (`C:\Apps\LFSoft\tools\sync\config\sync.yaml`). Si el servidor no tiene el sync en esa carpeta, pasar `-SyncInstallDir` o directamente `-DatabaseUrl`.

## Uso

Copiar la carpeta al servidor y, desde una consola de PowerShell en esa carpeta:

```powershell
.\Aplicar-Grillas.ps1 -Listar                  # grillas y columnas que deja visibles (no toca la base)
.\Aplicar-Grillas.ps1                          # todas
.\Aplicar-Grillas.ps1 -Grilla Customers, Quotes
.\Aplicar-Grillas.ps1 -DatabaseUrl "postgres://usuario:clave@localhost:5432/lfsoft_hidramic"
```

Si PowerShell no deja correr el script por la política de ejecución:

```powershell
powershell -ExecutionPolicy Bypass -File .\Aplicar-Grillas.ps1
```

Para cambiar qué columnas quedan visibles en una grilla, editar la lista `visibles` de su script (nombres de campo de `settings.grid_column_definitions`). Si un nombre no está en el catálogo, el script lo avisa y sigue.
