# Delivery Service

Go backend for the Delivery section of `Food_Ordering_API_Spec_Go_Microservices.pdf`. It owns port 8084, PostgreSQL database `delivery_db`, and the eight public APIs in section 7. No web frontend or ngrok is needed for local testing.

## Run locally

Requirements: Go 1.26+ and PostgreSQL 16+. From this directory:

1. Start PostgreSQL: `docker compose up -d delivery-db` (or provide your own PostgreSQL).
2. Copy `.env.example` to `.env`. Set `JWT_SECRET` and `INTERNAL_API_KEY` to the same values used by teammates. Keep `.env` private.
3. Load the environment variables. In PowerShell:

   ```powershell
   Get-Content .env | ForEach-Object {
     if ($_ -match '^([^#=]+)=(.*)$') { [Environment]::SetEnvironmentVariable($matches[1], $matches[2], 'Process') }
   }
   go run -buildvcs=false ./cmd/server
   ```

4. Check `http://localhost:8084/healthz`. Database migration runs on startup.
5. Run `go test ./...`.

Set `ORDER_SERVICE_URL`, `USER_SERVICE_URL`, and `RESTAURANT_SERVICE_URL` to teammate URLs when integrating. These can be LAN, ngrok, or deployed URLs; no source-code change is needed.

## API contract

| Method | Path | Role |
| --- | --- | --- |
| POST | `/api/v1/deliveries` | restaurant_owner (same restaurant), admin |
| GET | `/api/v1/deliveries` | rider (own jobs), admin |
| GET | `/api/v1/deliveries/{id}` | related rider, restaurant_owner, customer, admin |
| PUT | `/api/v1/deliveries/{id}/assign` | admin |
| PUT | `/api/v1/deliveries/{id}/status` | assigned rider, admin |
| GET | `/api/v1/deliveries/{id}/track` | related customer, rider, admin |
| GET | `/api/v1/riders/{id}/deliveries` | same rider, admin |
| GET | `/api/v1/admin/dashboard` | admin |

Protected requests require `Authorization: Bearer <JWT>`. The JWT must use HS256, the shared secret, UUID v4 `sub`, `role`, and `exp`. POST/PUT require `Content-Type: application/json`. Creation accepts an optional `Idempotency-Key` to make retries return the original delivery; `order_id` is unique even without a key. Lists support `page` (default 1), `limit` (default 20, maximum 100), and `status`. Successful responses have `success` and `data`; errors have `success`, `error`, `message`, and `details`.

Example create request:

```http
POST http://localhost:8084/api/v1/deliveries
Authorization: Bearer <restaurant-owner-token>
Content-Type: application/json
Idempotency-Key: order-44444444-4444-4444-8444-444444444444

{"order_id":"44444444-4444-4444-8444-444444444444","pickup_address":"123 ถนนสุขุมวิท","dropoff_address":"88/8 ถนนพระราม 4"}
```

The service checks that the order is `ready` before creating one delivery per order. Assignment changes `waiting_rider` to `assigned`. Status changes must follow `assigned -> picked_up -> on_the_way -> delivered`; `failed` can terminate an active job. List and detail permissions are enforced using ownership saved when the delivery is created. Coordinates in `/track` are `null` until GPS updates arrive. The PDF defines a read API but no location write API, so this project includes a separate service-to-service `PUT /api/v1/internal/deliveries/{id}/location` endpoint requiring `X-Internal-Key` and JSON `{"lat":13.7563,"lng":100.5018}`. It accepts updates only while the job is active. Poll `/track` for the latest saved position.

## Integration requirements to agree with teammates

The PDF's sample `GET /api/v1/orders/{id}` and `GET /api/v1/restaurants/{id}` payloads omit fields needed to check ownership. For delivery creation, Order Service must include `id`, `status`, `customer_id`, and `restaurant_id` in `data`. The teammate's Order Service at commit `6eeef585373e52ebc1daf39b67864977518d69a8` does expose those Order fields. Restaurant Service must include `owner_id` in `GET /api/v1/restaurants/{id}` `data`; that service was not present in the reviewed repo. Delivery Service rejects incomplete responses with 503 instead of guessing ownership. Admin assignment validates `rider_id` through User Service `GET /api/v1/users/{id}`, expecting `role: rider` and `status: active`.

The dashboard gets `meta.total_items` from the three list APIs in parallel and counts local deliveries. Restaurant Service must include all restaurants, including pending ones, for an admin caller if `total_restaurants` is meant to cover all statuses. If `date_from`/`date_to` is supplied, it walks those lists and reads each item's `created_at`; all three services must include that field for date-filtered dashboard calls. If any service fails or omits required data, the dashboard returns 503 rather than partial totals. The dashboard is a JSON endpoint; it is not a rendered web page.

The reviewed Order Service commit returns `count` but no `meta.total_items` from `GET /api/v1/orders`. Its detail response is compatible with delivery creation, but the dashboard will return 503 until Order Service adds the standard pagination metadata. This service deliberately keeps the shared API contract instead of silently treating an unpaginated `count` as equivalent. The reviewed repo contains only Order Service; User and Restaurant Service still need separate integration checks.

The service forwards both the caller's Bearer token and shared `X-Internal-Key` when calling teammates. The other services need to accept those headers on the stated endpoints. Use the same `JWT_SECRET` and `INTERNAL_API_KEY` across services. Only `delivery_db` is accessed directly; IDs from other services have no cross-database foreign keys.

## Database design reconciliation

The team's newer database-design sheet adds `assigned_at` and `picked_up_at` to `deliveries`. Both are now created (or added to an existing table) and set at the corresponding transitions. `order_id` remains unique, `rider_id` stays nullable until assignment, and cross-service IDs have no foreign keys. Timestamps use `TIMESTAMPTZ` to preserve the API's UTC/RFC 3339 behavior, while the design sheet labels them generically as `TIMESTAMP`.

This service also stores `customer_id`, `restaurant_id`, and `restaurant_owner_id` as snapshots for access checks, plus `lat` and `lng` for the tracking API. `idempotency_key`, `idempotency_actor_id`, and `request_hash` support safe retries. These extra columns are not listed in the design sheet but support requirements from the API specification. A fresh database has only the `deliveries` table shown in the design sheet. If an earlier version of this project created `idempotency_keys`, startup copies its replay data into `deliveries` without deleting the old table; remove the legacy table only after a database backup and review. The team should agree whether the sheet lists minimum fields or forbids additional columns before final submission.
