# PROMPTS.md

Verbatim log of the prompts that drive this project, one section per session. Newest at the bottom.

---

## 2026-10-25 — Session 1: kickoff & plan

### Prompt 1 (user)

> Hola, este proyecto va a estar enfocado en crear un flujo para tarjetas de crédito bancarias, donde las principales features que vamos a tener son las siguientes:
>
> - Collateral deposit: receiving and confirming the customer's guarantee deposit.
> - Card activation: a card becomes active only once its collateral is confirmed, with a credit limit derived from the deposit.
> - Spending: authorizing purchases against the available credit. A purchase must never exceed the limit.
> - Repayments: customers paying back what they've spent, which restores their available credit.
> - Collateral release or application: returning the deposit when an account is closed in good standing, or applying it to cover unpaid debt.
>
> El objetivo principal del proyecto es que estas sean las tarjetas de crédito seguras que funcionan teniendo ya un depósito por detrás para garantizar el pago de las compras que se hagan, primero que todo.
>
> Para este proyecto lo que vamos a crear es una arquitectura de microservicios conectados a cada uno de los servicios que vamos a tener aquí. Para todo el flow y toda la decisión de negocio usaremos Go para manejar todas las concurrencias de una mejor forma y poder ser un proyecto escalable muy fácilmente en el área de fintech.
> Para el manejo de la idempotencia vamos a utilizar primero un header que tenga una idempotency key. Va a ser una clave única por cada petición y va a ser con un SHA-256 para manejar la seguridad.
>
> Ahora para el tema de la concurrencia vamos a usar un insert first y, obviamente, para todo el tema de bases de datos usaremos PostgreSQL para hacer esto. Entonces aquí PostgreSQL va a tener que esperar hacia el siguiente insert. Se tiene como la misma clave esperando si fue con éxito o fallida. Podemos extender un replay para tener la mejor auditoría de esto. Dentro de esa auditoría vamos a guardar los códigos de estatus 200, 400, 500 o cualquier cosa que haya pasado: si fue un timeout o si tuvo que hacer un rollback por alguna decisión. Estos keys de impotencia tienen un tiempo válido de unas 24 horas y luego ya se van a ir limpiando. Por otro lado también tenemos que tener una cola SNS con el NID para que guardemos todos los eventos que se vayan a hacer dentro de la app. Para dar validez a esto el test principal que tenemos que pasar es hacer intentos concurrentes de la misma transacción en un corto período de tiempo y, al final, el resultado solo debe ser uno. Por ejemplo si intentamos hacer varios depósitos de lo mismo de la misma petición o varios descuentos de la misma petición para la app, esto es lo principal que tenemos que atender.
>
> Para todas las tareas principales anteriores que te pasé, vamos a manejar distintos estados.
>
> Por ejemplo la tarjeta solo va a manejar tres estados: pending colateral (o sea que está pendiente de hacer un depósito), luego active y por último closed.
>
> Ahora los movimientos sí van a tener sus estados internos que van a funcionar en base al estado de la tarjeta. Es decir esos movimientos solo van a poder pasar, en su mayoría, a estado de active.
>
> Los estados que vamos a tener van a ser:
> - `account closed` para las cuentas cerradas.
> - `lateral not confirmer` cuando sí queremos hacer un movimiento y aún no se ha confirmado el depósito.
> - `card frozen` si el cliente quiere congelar la tarjeta. Esto va a ser un filtro que también vamos a tener.
> Vamos a poner:
> - un rate límite o un velocity límite de intentos en una cantidad de tiempo.
> - un límite de compra que el cliente va a poder setear.
> También vamos a tener otro estado que será para manejar el sobregiro de las tarjetas. Vamos a poder tener que las tarjetas tengan un 10% de sobregiro, es decir, si se ha acabado el depósito o el cupo, puedan pagar un 10% más de lo que tienen. Otro evento va a ser `crédito insuficiente`.
>
> Esto, principalmente, ahí hablamos un poco de features que vamos a discutir. Luego de esto vamos a crear un plan de sprint development para discutir cada una de las features más a detalle y también vamos a setear el flujo de testeo que deben pasar luego de esto.
>
> Ahora en cuanto a seguridad, cómo vamos a manejarla: tendemos a tener siempre JWTHS 256. Cada token también va a tener authorized accounts, es decir, qué cuentas se autorizan para hacer los movimientos. Los webhooks que tengamos van a tener su firma correspondiente de HMAC y los rechazos de Wet Hut. Vamos a tratar de manejarlos para no guardar mucha basura: simplemente uno por dentro de un rango, dentro de un período de tiempo en que se reciban del mismo origen.
>
> Por ejemplo todos los cuerpos JSON dados no pueden dar información que no se esté utilizando en ese momento. Vamos a tener los dos endpoints para evaluar la salud de la app. Dentro de aquí vamos a tener que meter unos workers y algunos balanceadores de carga para distribuir todos los mensajes, haciendo una simulación de que tenemos unos 1,000 usuarios que usan activamente la cuenta.
>
> Para las bases de datos, esto va a tener que tener su auditoría. Esta auditoría no va a tener un delete de la tabla de negocios. Simplemente va a guardar todos los estados, los snapshots, los cambios que tuvo y va a ser solo append only.
>
> Vamos a tener también dos entornos: dev y prod. Y por otro lado, para la arquitectura vamos a sentearla con AWS usando Terraform. Por ahora la arquitectura no la vamos a desplegar. Lo vamos a hacer todo de forma local pero vamos a ir desarrollando todo esto. Por ejemplo para AWS vamos a usar TLS 1.3. Vamos a manejar los secretos con KMS. Vamos a usar un RDS multi-Z. Vamos a utilizar S3 objects.
>
> Y bueno bien suelto con esto, ya podemos ir dividiendo las features y podemos ir viendo cómo vamos a ir desarrollando cada una. Principalmente el orden que propongo yo es:
> 1. Una apertura y depósito de la garantía como primer proceso.
> 2. La autorización, gestura de esto y el refund que podrían tener las tarjetas.
> 3. Vamos a hacer todos los flujos de pagos.
> 4. Ya pasaremos a estados de cuenta, los cobros de intereses y los cobros por mora que tengamos.
> 5. Vamos a hacer todo lo que sean cierres de cuenta, congelaciones, cancelaciones, etcétera.
> 6. Ya vamos a hacer la inclusión de auditoría de la app o podemos ir haciéndola paralelamente por cada flujo que vayamos desarrollando.
> Pienso igual que la auditoría la podemos ir incluyendo desde el inicio para que sea un middleware automático. Puede ser pero bueno, esto sería lo principal.
> Vamos a hablar un poco al respecto. Si tienes alguna pregunta acerca del proceso, hágamela. Repito: eso no va a ser desplegado, va a funcionar en Docker. Quiero que también, de pronto, a medida que vayamos guardando, lo vayan guardando en el archivo .endy dentro del proyecto.

### Clarifications (user answers)

- Overdraft 10%: **not in MVP**. Hard limit = collateral.
- Services: **Go monorepo, 5 services + gateway** (accounts, ledger, billing, audit, notifications).
- AWS locally: **LocalStack in docker-compose**.
- ".endy" meant: **save the prompts in PROMPTS.md**.
- Internal protocol: **gRPC + protobuf**; REST/JSON only at the gateway.
- Ledger: **double-entry, append-only, int64 minor units**.
- Language: **everything in English**.
- First delivery: **skeleton + Sprint 1 complete**.
- Collateral confirmation: **simulated bank webhook** (HMAC).
- Local DB: **single Postgres**, schema per service.
- Currency: **multi-currency per customer**, one account per currency.
- JWT issuer: **dev-only token CLI**.
- Rate limit backend: **Postgres**.
- Collateral top-ups after activation: **yes**, limit grows.

### Prompt 2 (user)

> tenemos ya el plan paso ap aso con los test?

### Prompt 3 (user)

> Ok, tambien hay que agregar una pantalla en nextJs sencilla para simular los flujos y poder hacer todos los testeos correspondinetes

### Prompt 4 (user)

> Continua

---

## Sprint 1 delivery notes (2026-10-25)

Skeleton + Sprint 1 implemented with TDD: money, config, db, idempotency, auth, outbox/events, audit, ledger, accounts, gateway, simulator, e2e + k6, Terraform. See `docs/TESTING.md` for results and `docs/DECISIONS.md` ADR-012 for rulings made during implementation.

### Prompt 5 (user)

> Continua

---

## 2026-10-25 — Session 1 (cont.): Sprint 2 discovery

### Prompt 6 (user)

> Okay dale, entonces vamos a hablar del sprint 2. Dame las preguntas que son necesarias para completar todo eso. Si necesitas algo de contexto con respecto a la app, vamos a trabajar punto por punto: autorizaciones, capturas, refunds y razones de rechazo.

### Prompt 7 (user) — Sprint 2 answers

> Sí para la primera vamos a simular un endpoint firmado con HMAC. Va a vivir 7 días. No se van a aprobar los montos parciales. No va a haber autorizaciones incrementales. Vamos a guardar el ID, el nombre y la categoría. No vamos a hacer directamente las conversiones a la divisa base de la cuenta.
> Sí la captura va a ser igual o menos. No se van a permitir capturas parciales, no va a haber capturas sin autorización previa y se pueden hacer reversos en un lapso de 3 días de la transacción. Los refunds van sobre la compra capturada. No hay refunds en la compra original. Sí quedaría un saldo a favor para la tarjeta si llega refund después de que el cliente ya pagó con la tarjeta. Si ese orden está bien para testear, los intentos de compra van a ser 10 por minuto. Van a hacer otra transacción de compra. El congelar la tarjeta, podemos meterlo antes porque al final va a ser una opción para rechazo. Sí, dependiendo del caso va a ser un 201. Si es un caso manejado, por ejemplo, en el que está declinada la tarjeta o de pronto está congelada, se va a manejar como un 201 y se da la razón. Vamos a mover las notificaciones a otro sprint. El sobregiro lo vamos a quitar. Eso no se va a poder permitir.

### Prompt 8 (user) — Sprint 2 clarifications

> Saldo a favor: No, límite estricto. Otra moneda: Sí, razón nueva `currency_not_supported`. Interpretaciones (a)-(d): Sí, todas.

### Prompt 9 (user)

> Si, aprobado

## Sprint 2 delivery notes (2026-10-25)

Sprint 2 implemented on branch `sprint-2` from `docs/superpowers/plans/2026-10-25-sprint2-card-transactions.md`. Results in `docs/TESTING.md`, rulings in ADR-015.

### Prompt 10 (user)

> Si, aprobado (Sprint 2 design) — Sprint 2 implemented on branch `sprint-2`, final review fixes in ADR-016.
