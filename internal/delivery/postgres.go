package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type PostgresStore struct{ DB *sql.DB }

func OpenPostgres(ctx context.Context, dsn string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(15)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &PostgresStore{DB: db}, nil
}

func (p *PostgresStore) Migrate(ctx context.Context, sqlText string) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range strings.Split(sqlText, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	// Preserve idempotency replay data if an earlier local schema was started.
	var legacyExists bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('public.idempotency_keys') IS NOT NULL`).Scan(&legacyExists); err != nil {
		return err
	}
	if legacyExists {
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries d SET
 idempotency_key=k.key, idempotency_actor_id=k.actor_id, request_hash=k.request_hash
 FROM idempotency_keys k WHERE d.id=k.delivery_id AND d.idempotency_key IS NULL`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const columns = `id::text, order_id::text, customer_id::text, restaurant_id::text,
 restaurant_owner_id::text, rider_id::text, pickup_address, dropoff_address, status,
 lat, lng, assigned_at, picked_up_at, created_at, updated_at, delivered_at`

type scanner interface{ Scan(...any) error }

func scanDelivery(s scanner) (Delivery, error) {
	var d Delivery
	var rider sql.NullString
	var lat, lng sql.NullFloat64
	var assigned, pickedUp, delivered sql.NullTime
	err := s.Scan(&d.ID, &d.OrderID, &d.CustomerID, &d.RestaurantID,
		&d.RestaurantOwnerID, &rider, &d.PickupAddress, &d.DropoffAddress, &d.Status,
		&lat, &lng, &assigned, &pickedUp, &d.CreatedAt, &d.UpdatedAt, &delivered)
	if err != nil {
		return Delivery{}, err
	}
	d.CreatedAt = d.CreatedAt.UTC()
	d.UpdatedAt = d.UpdatedAt.UTC()
	if rider.Valid {
		d.RiderID = &rider.String
	}
	if lat.Valid {
		d.Lat = &lat.Float64
	}
	if lng.Valid {
		d.Lng = &lng.Float64
	}
	if assigned.Valid {
		t := assigned.Time.UTC()
		d.AssignedAt = &t
	}
	if pickedUp.Valid {
		t := pickedUp.Time.UTC()
		d.PickedUpAt = &t
	}
	if delivered.Valid {
		t := delivered.Time.UTC()
		d.DeliveredAt = &t
	}
	return d, nil
}

func (p *PostgresStore) Create(ctx context.Context, d Delivery, key, actor, hash string) (Delivery, bool, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, false, err
	}
	defer tx.Rollback()
	if key != "" {
		// Serialize requests carrying the same key, including concurrent retries.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, actor+":create:"+key); err != nil {
			return Delivery{}, false, err
		}
		var oldHash, oldID string
		err := tx.QueryRowContext(ctx, `SELECT request_hash, id::text FROM deliveries
 WHERE idempotency_key=$1 AND idempotency_actor_id=$2`, key, actor).Scan(&oldHash, &oldID)
		if err == nil {
			if oldHash != hash {
				return Delivery{}, false, ErrKeyConflict
			}
			found, err := scanDelivery(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM deliveries WHERE id=$1`, oldID))
			return found, true, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Delivery{}, false, err
		}
	}
	var idemKey, idemActor, requestHash any
	if key != "" {
		idemKey, idemActor, requestHash = key, actor, hash
	}
	d, err = scanDelivery(tx.QueryRowContext(ctx, `INSERT INTO deliveries
 (id,order_id,customer_id,restaurant_id,restaurant_owner_id,pickup_address,dropoff_address,status,
  idempotency_key,idempotency_actor_id,request_hash)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+columns,
		d.ID, d.OrderID, d.CustomerID, d.RestaurantID, d.RestaurantOwnerID,
		d.PickupAddress, d.DropoffAddress, WaitingRider, idemKey, idemActor, requestHash))
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return Delivery{}, false, ErrConflict
		}
		return Delivery{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Delivery{}, false, err
	}
	return d, false, nil
}

func (p *PostgresStore) Get(ctx context.Context, id string) (Delivery, error) {
	d, err := scanDelivery(p.DB.QueryRowContext(ctx, `SELECT `+columns+` FROM deliveries WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, ErrNotFound
	}
	return d, err
}

func (p *PostgresStore) List(ctx context.Context, f ListFilter) ([]Delivery, int64, error) {
	var where []string
	var args []any
	if f.Status != "" {
		args = append(args, f.Status)
		where = append(where, fmt.Sprintf("status=$%d", len(args)))
	}
	if f.RiderID != "" {
		args = append(args, f.RiderID)
		where = append(where, fmt.Sprintf("rider_id=$%d", len(args)))
	}
	condition := ""
	if len(where) > 0 {
		condition = " WHERE " + strings.Join(where, " AND ")
	}
	var total int64
	if err := p.DB.QueryRowContext(ctx, `SELECT count(*) FROM deliveries`+condition, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, f.Limit, (f.Page-1)*f.Limit)
	rows, err := p.DB.QueryContext(ctx, `SELECT `+columns+` FROM deliveries`+condition+
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]Delivery, 0)
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

func (p *PostgresStore) Assign(ctx context.Context, id, rider string) (Delivery, error) {
	d, err := scanDelivery(p.DB.QueryRowContext(ctx, `UPDATE deliveries SET rider_id=$2,status='assigned',assigned_at=now(),updated_at=now()
 WHERE id=$1 AND status='waiting_rider' RETURNING `+columns, id, rider))
	if err == nil {
		return d, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, err
	}
	if _, err := p.Get(ctx, id); err != nil {
		return Delivery{}, err
	}
	return Delivery{}, ErrConflict
}

func (p *PostgresStore) SetStatus(ctx context.Context, id, status string) (Delivery, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT status FROM deliveries WHERE id=$1 FOR UPDATE`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, ErrNotFound
	}
	if err != nil {
		return Delivery{}, err
	}
	if !NextStatus(current, status) {
		return Delivery{}, ErrInvalidTransition
	}
	d, err := scanDelivery(tx.QueryRowContext(ctx, `UPDATE deliveries SET status=$2,updated_at=now(),
 picked_up_at=CASE WHEN $2='picked_up' THEN now() ELSE picked_up_at END,
 delivered_at=CASE WHEN $2='delivered' THEN now() ELSE delivered_at END
 WHERE id=$1 RETURNING `+columns, id, status))
	if err != nil {
		return Delivery{}, err
	}
	if err = tx.Commit(); err != nil {
		return Delivery{}, err
	}
	return d, nil
}

func (p *PostgresStore) SetLocation(ctx context.Context, id string, lat, lng float64) (Delivery, error) {
	d, err := scanDelivery(p.DB.QueryRowContext(ctx, `UPDATE deliveries SET lat=$2,lng=$3,updated_at=now()
 WHERE id=$1 AND status IN ('assigned','picked_up','on_the_way') RETURNING `+columns, id, lat, lng))
	if err == nil {
		return d, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, err
	}
	if _, err := p.Get(ctx, id); err != nil {
		return Delivery{}, err
	}
	return Delivery{}, ErrConflict
}

func (p *PostgresStore) Count(ctx context.Context, from, to *time.Time) (int64, error) {
	var n int64
	err := p.DB.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE ($1::timestamptz IS NULL OR created_at >= $1)
 AND ($2::timestamptz IS NULL OR created_at <= $2)`, from, to).Scan(&n)
	return n, err
}
