package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type requestIDKey struct{}

type Principal struct{ ID, Role, Token string }

type Server struct {
	Store       Store
	Upstreams   Upstreams
	JWTSecret   []byte
	InternalKey string
	RateLimit   int
	Logger      *slog.Logger
	mu          sync.Mutex
	rate        map[string]rateEntry
}

type rateEntry struct {
	window time.Time
	count  int
}

func NewServer(store Store, upstreams Upstreams, jwtSecret, internalKey string, limit int, logger *slog.Logger) *Server {
	if limit <= 0 {
		limit = 120
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{Store: store, Upstreams: upstreams, JWTSecret: []byte(jwtSecret), InternalKey: internalKey,
		RateLimit: limit, Logger: logger, rate: make(map[string]rateEntry)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/deliveries", s.create)
	mux.HandleFunc("GET /api/v1/deliveries", s.list)
	mux.HandleFunc("GET /api/v1/deliveries/{id}", s.get)
	mux.HandleFunc("PUT /api/v1/deliveries/{id}/assign", s.assign)
	mux.HandleFunc("PUT /api/v1/deliveries/{id}/status", s.setStatus)
	mux.HandleFunc("GET /api/v1/deliveries/{id}/track", s.track)
	mux.HandleFunc("GET /api/v1/riders/{id}/deliveries", s.riderList)
	mux.HandleFunc("GET /api/v1/admin/dashboard", s.dashboard)
	mux.HandleFunc("PUT /api/v1/internal/deliveries/{id}/location", s.setLocation)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"success": true, "data": map[string]string{"status": "ok"}})
	})
	return s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		if pattern == "" {
			fail(w, 404, "NOT_FOUND", "path not found", nil)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID, X-Internal-Key")
		rid := r.Header.Get("X-Request-ID")
		if rid == "" || len(rid) > 128 {
			rid, _ = NewUUID()
		}
		w.Header().Set("X-Request-ID", rid)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, rid))
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !s.allow(clientIP(r)) {
			fail(w, 429, "TOO_MANY_REQUESTS", "rate limit exceeded", nil)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/internal/") && (s.InternalKey == "" || r.Header.Get("X-Internal-Key") != s.InternalKey) {
			fail(w, 403, "FORBIDDEN", "invalid internal key", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) allow(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.rate) > 10000 {
		for k, v := range s.rate {
			if now.Sub(v.window) > 2*time.Minute {
				delete(s.rate, k)
			}
		}
	}
	e := s.rate[ip]
	if now.Sub(e.window) >= time.Minute {
		e = rateEntry{window: now}
	}
	e.count++
	s.rate[ip] = e
	return e.count <= s.RateLimit
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, message string, details any) {
	write(w, status, map[string]any{"success": false, "error": code, "message": message, "details": details})
}

func success(w http.ResponseWriter, status int, data any) {
	write(w, status, map[string]any{"success": true, "data": data})
}

func (s *Server) auth(w http.ResponseWriter, r *http.Request, roles ...string) (Principal, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		fail(w, 401, "UNAUTHORIZED", "missing bearer token", nil)
		return Principal{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, errors.New("unexpected signing method")
		}
		return s.JWTSecret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !parsed.Valid {
		fail(w, 401, "UNAUTHORIZED", "invalid or expired token", nil)
		return Principal{}, false
	}
	id, _ := claims.GetSubject()
	role, _ := claims["role"].(string)
	if !UUID4(id) || role == "" {
		fail(w, 401, "UNAUTHORIZED", "invalid token claims", nil)
		return Principal{}, false
	}
	for _, allowed := range roles {
		if role == allowed {
			return Principal{id, role, token}, true
		}
	}
	fail(w, 403, "FORBIDDEN", "role is not allowed", nil)
	return Principal{}, false
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if media := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); media != "application/json" {
		fail(w, 400, "BAD_REQUEST", "Content-Type must be application/json", nil)
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		fail(w, 400, "BAD_REQUEST", "invalid JSON body", nil)
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		fail(w, 400, "BAD_REQUEST", "only one JSON value is allowed", nil)
		return false
	}
	return true
}

func idParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !UUID4(id) {
		fail(w, 422, "VALIDATION_ERROR", "id must be UUID v4", nil)
		return "", false
	}
	return id, true
}

func pageParams(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	page, limit := 1, 20
	var err error
	if v := r.URL.Query().Get("page"); v != "" {
		page, err = strconv.Atoi(v)
		if err != nil || page < 1 {
			fail(w, 422, "VALIDATION_ERROR", "page must be >= 1", nil)
			return 0, 0, false
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit < 1 || limit > 100 {
			fail(w, 422, "VALIDATION_ERROR", "limit must be between 1 and 100", nil)
			return 0, 0, false
		}
	}
	if page > int(^uint(0)>>1)/limit {
		fail(w, 422, "VALIDATION_ERROR", "page is too large", nil)
		return 0, 0, false
	}
	return page, limit, true
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.Logger.Error("request failed", "error", err)
	fail(w, 500, "INTERNAL_ERROR", "internal server error", nil)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth(w, r, "restaurant_owner", "admin")
	if !ok {
		return
	}
	var body struct {
		OrderID        string `json:"order_id"`
		PickupAddress  string `json:"pickup_address"`
		DropoffAddress string `json:"dropoff_address"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !UUID4(body.OrderID) || strings.TrimSpace(body.PickupAddress) == "" || strings.TrimSpace(body.DropoffAddress) == "" {
		fail(w, 422, "VALIDATION_ERROR", "order_id, pickup_address and dropoff_address are required", nil)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > 200 {
		fail(w, 422, "VALIDATION_ERROR", "Idempotency-Key must be at most 200 characters", nil)
		return
	}
	order, err := s.Upstreams.Order(r.Context(), body.OrderID, p.Token)
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "NOT_FOUND", "order not found", nil)
		return
	}
	if err != nil {
		fail(w, 503, "ORDER_SERVICE_UNAVAILABLE", "order service unavailable", nil)
		return
	}
	if order.Status != "ready" {
		fail(w, 400, "BAD_REQUEST", "order is not ready for delivery", map[string]string{"current_status": order.Status})
		return
	}
	owner, err := s.Upstreams.RestaurantOwner(r.Context(), order.RestaurantID, p.Token)
	if err != nil {
		fail(w, 503, "RESTAURANT_SERVICE_UNAVAILABLE", "restaurant service unavailable", nil)
		return
	}
	if p.Role == "restaurant_owner" && owner != p.ID {
		fail(w, 403, "FORBIDDEN", "not the restaurant owner", nil)
		return
	}
	id, err := NewUUID()
	if err != nil {
		s.internalError(w, err)
		return
	}
	d := Delivery{ID: id, OrderID: order.ID, CustomerID: order.CustomerID, RestaurantID: order.RestaurantID,
		RestaurantOwnerID: owner, PickupAddress: strings.TrimSpace(body.PickupAddress), DropoffAddress: strings.TrimSpace(body.DropoffAddress)}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", body.OrderID, d.PickupAddress, d.DropoffAddress)))
	created, replayed, err := s.Store.Create(r.Context(), d, key, p.ID, hex.EncodeToString(h[:]))
	switch {
	case errors.Is(err, ErrKeyConflict):
		fail(w, 409, "CONFLICT", "Idempotency-Key was used for a different request", nil)
	case errors.Is(err, ErrConflict):
		fail(w, 409, "CONFLICT", "delivery already exists for this order", nil)
	case err != nil:
		s.internalError(w, err)
	case replayed:
		success(w, 200, created)
	default:
		success(w, 201, created)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth(w, r, "rider", "admin")
	if !ok {
		return
	}
	page, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	f := ListFilter{Page: page, Limit: limit, Status: r.URL.Query().Get("status"), RiderID: r.URL.Query().Get("rider_id")}
	if f.Status != "" && !ValidStatus(f.Status) {
		fail(w, 422, "VALIDATION_ERROR", "invalid status", nil)
		return
	}
	if f.RiderID != "" && !UUID4(f.RiderID) {
		fail(w, 422, "VALIDATION_ERROR", "rider_id must be UUID v4", nil)
		return
	}
	if p.Role == "rider" {
		f.RiderID = p.ID
	}
	items, total, err := s.Store.List(r.Context(), f)
	if err != nil {
		s.internalError(w, err)
		return
	}
	write(w, 200, map[string]any{"success": true, "data": items, "meta": MakeMeta(page, limit, total)})
}

func (s *Server) getDelivery(w http.ResponseWriter, r *http.Request) (Delivery, bool) {
	id, ok := idParam(w, r)
	if !ok {
		return Delivery{}, false
	}
	d, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "NOT_FOUND", "delivery not found", nil)
		return Delivery{}, false
	}
	if err != nil {
		s.internalError(w, err)
		return Delivery{}, false
	}
	return d, true
}

func permitted(p Principal, d Delivery) bool {
	switch p.Role {
	case "admin":
		return true
	case "customer":
		return p.ID == d.CustomerID
	case "restaurant_owner":
		return p.ID == d.RestaurantOwnerID
	case "rider":
		return d.RiderID != nil && p.ID == *d.RiderID
	default:
		return false
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth(w, r, "rider", "restaurant_owner", "customer", "admin")
	if !ok {
		return
	}
	d, ok := s.getDelivery(w, r)
	if !ok {
		return
	}
	if !permitted(p, d) {
		fail(w, 403, "FORBIDDEN", "not related to this delivery", nil)
		return
	}
	success(w, 200, d)
}

func (s *Server) assign(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth(w, r, "admin")
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var body struct {
		RiderID string `json:"rider_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !UUID4(body.RiderID) {
		fail(w, 422, "VALIDATION_ERROR", "rider_id must be UUID v4", nil)
		return
	}
	valid, err := s.Upstreams.Rider(r.Context(), body.RiderID, p.Token)
	if errors.Is(err, ErrNotFound) || (err == nil && !valid) {
		fail(w, 422, "VALIDATION_ERROR", "rider_id must identify an active rider", nil)
		return
	}
	if err != nil {
		fail(w, 503, "USER_SERVICE_UNAVAILABLE", "user service unavailable", nil)
		return
	}
	d, err := s.Store.Assign(r.Context(), id, body.RiderID)
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "NOT_FOUND", "delivery not found", nil)
	case errors.Is(err, ErrConflict):
		fail(w, 409, "CONFLICT", "delivery is already assigned or closed", nil)
	case err != nil:
		s.internalError(w, err)
	default:
		success(w, 200, d)
	}
}

func (s *Server) setStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth(w, r, "rider", "admin")
	if !ok {
		return
	}
	d, ok := s.getDelivery(w, r)
	if !ok {
		return
	}
	if p.Role == "rider" && !permitted(p, d) {
		fail(w, 403, "FORBIDDEN", "not the assigned rider", nil)
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Status != PickedUp && body.Status != OnTheWay && body.Status != Delivered && body.Status != Failed {
		fail(w, 422, "VALIDATION_ERROR", "invalid delivery status", nil)
		return
	}
	updated, err := s.Store.SetStatus(r.Context(), d.ID, body.Status)
	switch {
	case errors.Is(err, ErrInvalidTransition):
		fail(w, 409, "INVALID_STATUS_TRANSITION", "cannot skip or repeat delivery status", map[string]string{"current_status": d.Status})
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "NOT_FOUND", "delivery not found", nil)
	case err != nil:
		s.internalError(w, err)
	default:
		success(w, 200, updated)
	}
}

func (s *Server) track(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth(w, r, "customer", "rider", "admin")
	if !ok {
		return
	}
	d, ok := s.getDelivery(w, r)
	if !ok {
		return
	}
	if !permitted(p, d) {
		fail(w, 403, "FORBIDDEN", "not related to this delivery", nil)
		return
	}
	success(w, 200, map[string]any{"id": d.ID, "lat": d.Lat, "lng": d.Lng, "status": d.Status, "updated_at": d.UpdatedAt})
}

func (s *Server) setLocation(w http.ResponseWriter, r *http.Request) {
	// Internal GPS ingest is not one of the eight public endpoints. The shared
	// internal key is checked by middleware before this handler is reached.
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Lat *float64 `json:"lat"`
		Lng *float64 `json:"lng"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Lat == nil || body.Lng == nil || math.IsNaN(*body.Lat) || math.IsNaN(*body.Lng) ||
		*body.Lat < -90 || *body.Lat > 90 || *body.Lng < -180 || *body.Lng > 180 {
		fail(w, 422, "VALIDATION_ERROR", "lat and lng are required in valid ranges", nil)
		return
	}
	d, err := s.Store.SetLocation(r.Context(), id, *body.Lat, *body.Lng)
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "NOT_FOUND", "delivery not found", nil)
	case errors.Is(err, ErrConflict):
		fail(w, 409, "CONFLICT", "delivery is not active", nil)
	case err != nil:
		s.internalError(w, err)
	default:
		success(w, 200, map[string]any{"id": d.ID, "lat": d.Lat, "lng": d.Lng, "status": d.Status})
	}
}

func (s *Server) riderList(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth(w, r, "rider", "admin")
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if p.Role == "rider" && p.ID != id {
		fail(w, 403, "FORBIDDEN", "not your delivery history", nil)
		return
	}
	page, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	f := ListFilter{Page: page, Limit: limit, RiderID: id, Status: r.URL.Query().Get("status")}
	if f.Status != "" && !ValidStatus(f.Status) {
		fail(w, 422, "VALIDATION_ERROR", "invalid status", nil)
		return
	}
	items, total, err := s.Store.List(r.Context(), f)
	if err != nil {
		s.internalError(w, err)
		return
	}
	write(w, 200, map[string]any{"success": true, "data": items, "meta": MakeMeta(page, limit, total)})
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth(w, r, "admin")
	if !ok {
		return
	}
	var from, to *time.Time
	if v := r.URL.Query().Get("date_from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			fail(w, 422, "VALIDATION_ERROR", "date_from must be RFC 3339", nil)
			return
		}
		from = &t
	}
	if v := r.URL.Query().Get("date_to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			fail(w, 422, "VALIDATION_ERROR", "date_to must be RFC 3339", nil)
			return
		}
		to = &t
	}
	if from != nil && to != nil && from.After(*to) {
		fail(w, 422, "VALIDATION_ERROR", "date_from must be <= date_to", nil)
		return
	}
	type localResult struct {
		count int64
		err   error
	}
	ch := make(chan localResult, 1)
	go func() { n, err := s.Store.Count(r.Context(), from, to); ch <- localResult{n, err} }()
	users, restaurants, orders, err := s.Upstreams.DashboardCounts(r.Context(), p.Token, from, to)
	local := <-ch
	if err != nil || local.err != nil {
		s.Logger.Error("dashboard dependency failed", "upstream_error", err, "database_error", local.err)
		fail(w, 503, "INTERNAL_ERROR", "dashboard data unavailable", nil)
		return
	}
	success(w, 200, map[string]int64{"total_users": users, "total_restaurants": restaurants, "total_orders": orders, "total_deliveries": local.count})
}
