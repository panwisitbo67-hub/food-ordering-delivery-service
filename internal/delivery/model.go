package delivery

import (
	"context"
	"time"
)

type Delivery struct {
	ID                string     `json:"id"`
	OrderID           string     `json:"order_id"`
	CustomerID        string     `json:"-"`
	RestaurantID      string     `json:"-"`
	RestaurantOwnerID string     `json:"-"`
	RiderID           *string    `json:"rider_id"`
	PickupAddress     string     `json:"pickup_address"`
	DropoffAddress    string     `json:"dropoff_address"`
	Status            string     `json:"status"`
	Lat               *float64   `json:"lat"`
	Lng               *float64   `json:"lng"`
	AssignedAt        *time.Time `json:"assigned_at"`
	PickedUpAt        *time.Time `json:"picked_up_at"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	DeliveredAt       *time.Time `json:"delivered_at"`
}

type ListFilter struct {
	Status  string
	RiderID string
	Page    int
	Limit   int
}

type PageMeta struct {
	Page       int   `json:"page"`
	Limit      int   `json:"limit"`
	TotalItems int64 `json:"total_items"`
	TotalPages int64 `json:"total_pages"`
}

type Store interface {
	Create(context.Context, Delivery, string, string, string) (Delivery, bool, error)
	Get(context.Context, string) (Delivery, error)
	List(context.Context, ListFilter) ([]Delivery, int64, error)
	Assign(context.Context, string, string) (Delivery, error)
	SetStatus(context.Context, string, string) (Delivery, error)
	SetLocation(context.Context, string, float64, float64) (Delivery, error)
	Count(context.Context, *time.Time, *time.Time) (int64, error)
}

const (
	WaitingRider = "waiting_rider"
	Assigned     = "assigned"
	PickedUp     = "picked_up"
	OnTheWay     = "on_the_way"
	Delivered    = "delivered"
	Failed       = "failed"
)

func ValidStatus(s string) bool {
	switch s {
	case WaitingRider, Assigned, PickedUp, OnTheWay, Delivered, Failed:
		return true
	default:
		return false
	}
}

func NextStatus(from, to string) bool {
	if to == Failed {
		return from != Delivered && from != Failed
	}
	switch from {
	case Assigned:
		return to == PickedUp
	case PickedUp:
		return to == OnTheWay
	case OnTheWay:
		return to == Delivered
	default:
		return false
	}
}

func MakeMeta(page, limit int, total int64) PageMeta {
	return PageMeta{Page: page, Limit: limit, TotalItems: total, TotalPages: (total + int64(limit) - 1) / int64(limit)}
}
