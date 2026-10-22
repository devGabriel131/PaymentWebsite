# Acuerdo de Pago, Términos y Condiciones y Planes de Pago (Stripe)

Documento de referencia para la implementación del flujo de aceptación del **Acuerdo de Pago (Términos y Condiciones)** y la **Selección de Plan de Pago con Stripe**.

---

## 1. Flujo del Cliente y Ubicación en la Aplicación

El flujo del enlace del cliente (`/customer/{token}`) sigue este orden estricto:

1. **Bienvenida** (`GET /customer/{token}`)
2. **Información del estudiante** (`GET /customer/{token}/forms` → `POST /customer/{token}`)
3. **Acuerdo de Pago y Términos y Condiciones** (con casilla de aceptación obligatoria)
4. **Selección del Plan de Pago** (3 opciones transparentes con fechas y montos exactos en español → redirección al Payment Link de Stripe correspondiente)

Ningún cliente puede avanzar a la selección de pago ni a Stripe sin haber marcado y enviado la aceptación de los Términos y Condiciones (validado tanto en el cliente como en el servidor).

---

## 2. Requisitos de Administración (Management) y Base de Datos

### 2.1 Fecha de comienzo en Management
- En la página principal de administración (`/management/`), **arriba del registro de enlaces (`Link registry`)**, debe existir un control para configurar la **Fecha de comienzo** del programa (por ejemplo: `30 de septiembre de 2026`).
- Esta fecha se inserta dinámicamente en el encabezado del Acuerdo de Pago y aplica a la cláusula (2) de los Términos y Condiciones.

### 2.2 Estado activo del estudiante (`is_active`)
- La tabla de estudiantes (`students`) debe incluir el campo booleano `is_active` (`NOT NULL DEFAULT true`).
- Cuando un estudiante es dado de baja por falta de pago tras el periodo de gracia de 2 semanas, su estado cambia a `is_active = false`.
- **Regla estricta**: **Nunca se eliminan registros** de estudiantes ni de enlaces al dar de baja a un estudiante.

### 2.3 Registro de aceptación del acuerdo (`terms_accepted_at` y `program_start_date`)
- Para respaldo ante disputas en Stripe (contracargos) o agencias de cobro, se debe guardar en base de datos en el registro del enlace/estudiante:
  - **`terms_accepted_at` (`TIMESTAMPTZ`)**: Fecha y hora exacta en que el cliente marcó la casilla y aceptó los Términos y Condiciones (`NULL` mientras no haya aceptado; requerido para avanzar a pago).
  - **`program_start_date` (`TEXT` o `DATE`)**: Copia fija (*snapshot*) de la **Fecha de comienzo** vigente al momento en que el cliente aceptó el acuerdo, de modo que cambios futuros en Management para próximas matrículas no alteren la fecha a la que se comprometió cada estudiante bajo la cláusula (2).

---

## 3. Texto Oficial: Acuerdo de Pago y Términos y Condiciones

El siguiente texto reemplaza la versión anterior de ATH Móvil / WhatsApp y está adaptado a cobros mediante Stripe. Debe mostrarse en español claro, con tipografía estándar (sin caracteres Unicode decorativos):

### Encabezado del acuerdo

> **Programa de Preparación para el ASVAB — Masterminds Programa ASVAB**  
> **Fecha de comienzo:** `[Fecha de comienzo configurada en Management, ej. 30 de septiembre de 2026]`
>
> Aquí se presenta la información y los términos para la separación de su espacio en el Programa de preparación para el ASVAB, ofrecido por Masterminds Programa ASVAB.

### Términos y Condiciones (FAVOR DE LEER DETALLADAMENTE)

> **(1)** La matrícula solicitada será exclusiva para el estudiante matriculado.
>
> **(2)** El espacio de matrícula es exclusivo para la fecha de comienzo detallada en este acuerdo (**`[Fecha de comienzo]`**).
>
> **(3)** Mediante la aceptación de este acuerdo y el pago inicial o total, acepto los términos y condiciones del programa, al igual que el plan de pago seleccionado.
>
> **(4)** En los planes de pagos a plazos, los cobros se procesan de forma automática en las fechas acordadas. Si un pago no puede procesarse en la fecha correspondiente, el sistema reintentará el cobro automáticamente. De no recibirse el pago adeudado dentro de un periodo máximo de **dos (2) semanas (14 días)** a partir de la fecha de pago acordada, el estudiante matriculado será dado de baja automáticamente del programa.
>
> **(5)** En caso de que el estudiante sea dado de baja por falta de pago, para ser reinstalado en el programa deberá cubrir el pago atrasado original más un **cargo de reinstalación de $25.00**.
>
> **(6)** El pago inicial, pago parcial o pago en su totalidad **no será devuelto bajo ningún concepto**.
>
> **(7)** **SI EL ESTUDIANTE DECIDE DARSE DE BAJA DEL PROGRAMA EN CUALQUIER MOMENTO ANTES DE CUBRIR EL TOTAL DEL ACUERDO DE PAGO (MATRÍCULA), EL ESTUDIANTE SIGUE SIENDO RESPONSABLE DEL BALANCE ADEUDADO DE LA CANTIDAD TOTAL DE LA MATRÍCULA.**
>
> **(8)** Cualquier deuda total y/o parcial puede ser reportada a una agencia de cobro de no ser cumplido el acuerdo de pago aquí detallado.
>
> Cualquier duda o pregunta no dude en contactarnos. ¡Bienvenido/a a la familia de Masterminds Programa ASVAB!

### Casilla de Aceptación Obligatoria (Checkbox)

Debajo de los Términos y Condiciones debe aparecer una casilla obligatoria (`required`) antes del botón para continuar:

- **Etiqueta del checkbox**:  
  `He leído detalladamente y acepto los Términos y Condiciones y el Acuerdo de Pago del programa.`
- **Botón de acción**:  
  `Continuar a las opciones de pago`
- **Comportamiento**: El botón no permite avanzar si la casilla no está marcada. Al enviarse, el servidor guarda `terms_accepted_at` y el *snapshot* de `program_start_date` antes de mostrar o permitir el paso a las opciones de pago.

---

## 4. Página de Selección de Plan de Pago (Transparencia de Cobros)

Inmediatamente después de aceptar el Acuerdo de Pago, el cliente pasa a la página de **Selección de Plan de Pago** (todo en español).

- Los enlaces de pago (`Payment Links`) se configuran directamente en el panel de Stripe; la aplicación redirige al cliente al enlace de Stripe correspondiente a la opción elegida.
- **Regla de fechas**:
  - El **primer pago** corresponde al día en que el cliente realiza el pago (fecha actual en zona horaria de Puerto Rico, `America/Puerto_Rico`).
  - Los **pagos siguientes** se cobran mensualmente en el **mismo día calendario de cada mes subsiguiente** (`+1 mes`, `+2 meses`, etc.; si el día inicial es 29, 30 o 31 y un mes subsiguiente tiene menos días, se ajusta al último día de ese mes, igual que el ciclo mensual de Stripe).
- **Transparencia obligatoria**: Cada opción debe mostrar claramente el costo total, la cantidad de pagos y el desglose exacto de **cada fecha de cobro y su monto en dólares** antes de que el usuario seleccione el plan.

### Resumen de las 3 Opciones de Pago

| Opción | Nombre en UI (Español) | Total | Cantidad de Pagos | Desglose de Montos |
|---|---|---|---|---|
| **1** | **Pago Único** | **$600.00** | 1 pago | 1 pago de **$600.00** |
| **2** | **Plan de 3 Pagos Mensuales** | **$625.00** | 3 pagos | Pago 1 de **$208.34**; Pagos 2 y 3 de **$208.33** |
| **3** | **Plan de 6 Pagos Mensuales** | **$650.00** | 6 pagos | Pago 1 de **$108.35**; Pagos 2 al 6 de **$108.33** |

---

### Estructura de Desglose por Opción (Ejemplo con fecha de pago `21/10/2026`)

#### Opción 1: Pago Único — $600.00
- **Total a pagar:** $600.00
- **Calendario de cobro:**
  - **Pago único (Hoy — 21/10/2026):** $600.00

#### Opción 2: Plan de 3 Pagos — $625.00
- **Total a pagar:** $625.00 (dividido en 3 pagos mensuales)
- **Calendario de cobros automáticos:**
  - **1.er pago (Hoy — 21/10/2026):** $208.34
  - **2.do pago (21/11/2026):** $208.33
  - **3.er pago (21/12/2026):** $208.33

#### Opción 3: Plan de 6 Pagos — $650.00
- **Total a pagar:** $650.00 (dividido en 6 pagos mensuales)
- **Calendario de cobros automáticos:**
  - **1.er pago (Hoy — 21/10/2026):** $108.35
  - **2.do pago (21/11/2026):** $108.33
  - **3.er pago (21/12/2026):** $108.33
  - **4.to pago (21/01/2027):** $108.33
  - **5.to pago (21/02/2027):** $108.33
  - **6.to pago (21/03/2027):** $108.33
