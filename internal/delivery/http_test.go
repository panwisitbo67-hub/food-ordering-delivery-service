package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	ownerID      = "11111111-1111-4111-8111-111111111111"
	otherID      = "22222222-2222-4222-8222-222222222222"
	orderID      = "44444444-4444-4444-8444-444444444444"
	restaurantID = "33333333-3333-4333-8333-333333333333"
	riderID      = "66666666-6666-4666-8666-666666666666"
)

type memoryStore struct {
	mu    sync.Mutex
	items map[string]Delivery
	keys  map[string]string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{items: map[string]Delivery{}, keys: map[string]string{}}
}

func (m *memoryStore) Create(_ context.Context, d Delivery, key, actor, hash string) (Delivery, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.keys[actor+key]; ok {
		if m.keys[actor+key+":hash"] != hash {
			return Delivery{}, false, ErrKeyConflict
		}
		return m.items[id], true, nil
	}
	for _, existing := range m.items {
		if existing.OrderID == d.OrderID {
			return Delivery{}, false, ErrConflict
		}
	}
	d.Status = WaitingRider
	d.CreatedAt = time.Now().UTC()
	d.UpdatedAt = d.CreatedAt
	m.items[d.ID] = d
	m.keys[actor+key] = d.ID
	m.keys[actor+key+":hash"] = hash
	return d, false, nil
}
func (m *memoryStore) Get(_ context.Context, id string) (Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.items[id]
	if !ok {
		return Delivery{}, ErrNotFound
	}
	return d, nil
}
func (m *memoryStore) List(_ context.Context, f ListFilter) ([]Delivery, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := []Delivery{}
	for _, d := range m.items {
		if (f.Status == "" || f.Status == d.Status) && (f.RiderID == "" || (d.RiderID != nil && *d.RiderID == f.RiderID)) {
			items = append(items, d)
		}
	}
	return items, int64(len(items)), nil
}
func (m *memoryStore) Assign(_ context.Context, id, rider string) (Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.items[id]
	if !ok {
		return Delivery{}, ErrNotFound
	}
	if d.Status != WaitingRider {
		return Delivery{}, ErrConflict
	}
	d.RiderID = &rider
	d.Status = Assigned
	m.items[id] = d
	return d, nil
}
func (m *memoryStore) SetStatus(_ context.Context, id, status string) (Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.items[id]
	if !ok {
		return Delivery{}, ErrNotFound
	}
	if !NextStatus(d.Status, status) {
		return Delivery{}, ErrInvalidTransition
	}
	d.Status = status
	m.items[id] = d
	return d, nil
}
func (m *memoryStore) SetLocation(_ context.Context, id string, lat, lng float64) (Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.items[id]
	if !ok {
		return Delivery{}, ErrNotFound
	}
	if d.Status != Assigned && d.Status != PickedUp && d.Status != OnTheWay {
		return Delivery{}, ErrConflict
	}
	d.Lat = &lat
	d.Lng = &lng
	m.items[id] = d
	return d, nil
}
func (m *memoryStore) Count(_ context.Context, _, _ *time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.items)), nil
}

type fakeUpstream struct {
	orderStatus string
	fail        bool
}

func (f fakeUpstream) Order(_ context.Context, id, _ string) (OrderInfo, error) {
	if f.fail {
		return OrderInfo{}, ErrUpstream
	}
	return OrderInfo{ID: id, Status: f.orderStatus, CustomerID: otherID, RestaurantID: restaurantID}, nil
}
func (f fakeUpstream) RestaurantOwner(context.Context, string, string) (string, error) {
	return ownerID, nil
}
func (f fakeUpstream) Rider(context.Context, string, string) (bool, error) { return true, nil }
func (f fakeUpstream) DashboardCounts(context.Context, string, *time.Time, *time.Time) (int64, int64, int64, error) {
	if f.fail {
		return 0, 0, 0, ErrUpstream
	}
	return 3, 4, 5, nil
}

func signed(t *testing.T, id, role string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": id, "role": role, "exp": time.Now().Add(time.Hour).Unix()})
	s, err := token.SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func request(h http.Handler, method, path, token, body, key string, internal bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if internal {
		r.Header.Set("X-Internal-Key", "internal-secret")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func dataID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.Data.ID
}
func newHandler(store Store, up Upstreams) http.Handler {
	return NewServer(store, up, "test-secret", "internal-secret", 1000, nil).Handler()
}

func TestDeliveryLifecycleAndPermissions(t *testing.T) {
	store := newMemoryStore()
	h := newHandler(store, fakeUpstream{orderStatus: "ready"})
	owner := signed(t, ownerID, "restaurant_owner")
	admin := signed(t, ownerID, "admin")
	rider := signed(t, riderID, "rider")
	customer := signed(t, otherID, "customer")
	body := `{"order_id":"` + orderID + `","pickup_address":"Restaurant","dropoff_address":"Customer"}`
	create := request(h, "POST", "/api/v1/deliveries", owner, body, "first-order", false)
	if create.Code != 201 {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	id := dataID(t, create)
	replay := request(h, "POST", "/api/v1/deliveries", owner, body, "first-order", false)
	if replay.Code != 200 || dataID(t, replay) != id {
		t.Fatalf("idempotent replay: %d %s", replay.Code, replay.Body.String())
	}
	conflict := request(h, "POST", "/api/v1/deliveries", owner, strings.Replace(body, "Customer", "Changed", 1), "first-order", false)
	if conflict.Code != 409 {
		t.Fatalf("key conflict: %d", conflict.Code)
	}
	forbidden := request(h, "GET", "/api/v1/deliveries/"+id, signed(t, riderID, "customer"), "", "", false)
	if forbidden.Code != 403 {
		t.Fatalf("unrelated customer: %d", forbidden.Code)
	}
	assign := request(h, "PUT", "/api/v1/deliveries/"+id+"/assign", admin, `{"rider_id":"`+riderID+`"}`, "", false)
	if assign.Code != 200 {
		t.Fatalf("assign: %d %s", assign.Code, assign.Body.String())
	}
	skip := request(h, "PUT", "/api/v1/deliveries/"+id+"/status", rider, `{"status":"delivered"}`, "", false)
	if skip.Code != 409 || !strings.Contains(skip.Body.String(), "INVALID_STATUS_TRANSITION") {
		t.Fatalf("skip: %d %s", skip.Code, skip.Body.String())
	}
	for _, status := range []string{PickedUp, OnTheWay} {
		got := request(h, "PUT", "/api/v1/deliveries/"+id+"/status", rider, `{"status":"`+status+`"}`, "", false)
		if got.Code != 200 {
			t.Fatalf("status %s: %d %s", status, got.Code, got.Body.String())
		}
	}
	location := request(h, "PUT", "/api/v1/internal/deliveries/"+id+"/location", "", `{"lat":13.7563,"lng":100.5018}`, "", true)
	if location.Code != 200 {
		t.Fatalf("location: %d %s", location.Code, location.Body.String())
	}
	track := request(h, "GET", "/api/v1/deliveries/"+id+"/track", customer, "", "", false)
	if track.Code != 200 || !strings.Contains(track.Body.String(), "13.7563") {
		t.Fatalf("track: %d %s", track.Code, track.Body.String())
	}
	missingKey := request(h, "PUT", "/api/v1/internal/deliveries/"+id+"/location", "", `{"lat":0,"lng":0}`, "", false)
	if missingKey.Code != 403 {
		t.Fatalf("internal key: %d", missingKey.Code)
	}
}

func TestUnavailableOrderAndJSON404(t *testing.T) {
	h := newHandler(newMemoryStore(), fakeUpstream{fail: true})
	owner := signed(t, ownerID, "restaurant_owner")
	body := `{"order_id":"` + orderID + `","pickup_address":"A","dropoff_address":"B"}`
	w := request(h, "POST", "/api/v1/deliveries", owner, body, "one", false)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "ORDER_SERVICE_UNAVAILABLE") {
		t.Fatalf("order outage: %d %s", w.Code, w.Body.String())
	}
	notFound := request(h, "GET", "/does-not-exist", "", "", "", false)
	if notFound.Code != 404 || !strings.Contains(notFound.Body.String(), `"error":"NOT_FOUND"`) {
		t.Fatalf("404: %d %s", notFound.Code, notFound.Body.String())
	}
	options := request(h, "OPTIONS", "/anything", "", "", "", false)
	if options.Code != 204 {
		t.Fatalf("OPTIONS: %d", options.Code)
	}
}

func TestDashboardAllOrNothing(t *testing.T) {
	admin := signed(t, ownerID, "admin")
	for _, tc := range []struct {
		fail   bool
		status int
	}{{false, 200}, {true, 503}} {
		h := newHandler(newMemoryStore(), fakeUpstream{fail: tc.fail})
		w := request(h, "GET", "/api/v1/admin/dashboard", admin, "", "", false)
		if w.Code != tc.status {
			t.Fatalf("dashboard fail=%v: %d %s", tc.fail, w.Code, w.Body.String())
		}
		if tc.fail && strings.Contains(w.Body.String(), "total_users") {
			t.Fatal("partial dashboard data leaked")
		}
	}
}

func TestUpstreamIncompleteOwnershipRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/orders/") {
			io.WriteString(w, `{"data":{"id":"`+orderID+`","status":"ready"}}`)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	u := &HTTPUpstreams{Client: srv.Client(), OrderURL: srv.URL, RestaurantURL: srv.URL, UserURL: srv.URL}
	_, err := u.Order(context.Background(), orderID, "token")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("missing ownership fields: %v", err)
	}
}

func TestDashboardHTTPCountsAndMissingMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-token" || r.Header.Get("X-Internal-Key") != "internal-secret" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/users":
			io.WriteString(w, `{"data":[],"meta":{"total_items":12}}`)
		case "/api/v1/restaurants":
			io.WriteString(w, `{"data":[],"meta":{"total_items":3}}`)
		case "/api/v1/orders":
			io.WriteString(w, `{"data":[],"meta":{"total_items":42}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	u := &HTTPUpstreams{Client: server.Client(), OrderURL: server.URL, RestaurantURL: server.URL, UserURL: server.URL, InternalKey: "internal-secret"}
	users, restaurants, orders, err := u.DashboardCounts(context.Background(), "admin-token", nil, nil)
	if err != nil || users != 12 || restaurants != 3 || orders != 42 {
		t.Fatalf("counts: %d %d %d, %v", users, restaurants, orders, err)
	}
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[]}`)
	}))
	defer missing.Close()
	u.UserURL = missing.URL
	_, _, _, err = u.DashboardCounts(context.Background(), "admin-token", nil, nil)
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("missing meta must fail: %v", err)
	}
}
