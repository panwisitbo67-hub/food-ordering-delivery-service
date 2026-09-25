CREATE TABLE IF NOT EXISTS deliveries (
    id UUID PRIMARY KEY,
    order_id UUID NOT NULL UNIQUE,
    customer_id UUID NOT NULL,
    restaurant_id UUID NOT NULL,
    restaurant_owner_id UUID NOT NULL,
    rider_id UUID,
    pickup_address TEXT NOT NULL,
    dropoff_address TEXT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'waiting_rider'
      CHECK (status IN ('waiting_rider','assigned','picked_up','on_the_way','delivered','failed')),
    lat DOUBLE PRECISION,
    lng DOUBLE PRECISION,
    idempotency_key TEXT,
    idempotency_actor_id UUID,
    request_hash TEXT,
    assigned_at TIMESTAMPTZ,
    picked_up_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at TIMESTAMPTZ,
    CHECK ((lat IS NULL AND lng IS NULL) OR (lat BETWEEN -90 AND 90 AND lng BETWEEN -180 AND 180))
);
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS assigned_at TIMESTAMPTZ;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS picked_up_at TIMESTAMPTZ;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS idempotency_actor_id UUID;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS request_hash TEXT;
ALTER TABLE deliveries ALTER COLUMN status TYPE VARCHAR(20);
CREATE INDEX IF NOT EXISTS deliveries_rider_created_idx ON deliveries (rider_id, created_at DESC);
CREATE INDEX IF NOT EXISTS deliveries_status_created_idx ON deliveries (status, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS deliveries_idempotency_idx
    ON deliveries (idempotency_key, idempotency_actor_id)
    WHERE idempotency_key IS NOT NULL;
