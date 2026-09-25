package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrUpstream = errors.New("upstream service unavailable")

type OrderInfo struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	CustomerID   string `json:"customer_id"`
	RestaurantID string `json:"restaurant_id"`
}

type Upstreams interface {
	Order(context.Context, string, string) (OrderInfo, error)
	RestaurantOwner(context.Context, string, string) (string, error)
	Rider(context.Context, string, string) (bool, error)
	DashboardCounts(context.Context, string, *time.Time, *time.Time) (int64, int64, int64, error)
}

type HTTPUpstreams struct {
	Client                           *http.Client
	OrderURL, RestaurantURL, UserURL string
	InternalKey                      string
}

func (u *HTTPUpstreams) get(ctx context.Context, base, path, token string, dst any) (int, error) {
	endpoint, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Internal-Key", u.InternalKey)
	if rid, ok := ctx.Value(requestIDKey{}).(string); ok {
		req.Header.Set("X-Request-ID", rid)
	}
	res, err := u.Client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return res.StatusCode, nil
	}
	dec := json.NewDecoder(io.LimitReader(res.Body, 2<<20))
	if err = dec.Decode(dst); err != nil {
		return 0, fmt.Errorf("%w: invalid JSON: %v", ErrUpstream, err)
	}
	return 200, nil
}

func (u *HTTPUpstreams) Order(ctx context.Context, id, token string) (OrderInfo, error) {
	var envelope struct {
		Data OrderInfo `json:"data"`
	}
	status, err := u.get(ctx, u.OrderURL, "/api/v1/orders/"+url.PathEscape(id), token, &envelope)
	if err != nil {
		return OrderInfo{}, err
	}
	if status == 404 {
		return OrderInfo{}, ErrNotFound
	}
	if status != 200 {
		return OrderInfo{}, ErrUpstream
	}
	d := envelope.Data
	if d.ID != id || !UUID4(d.CustomerID) || !UUID4(d.RestaurantID) || d.Status == "" {
		return OrderInfo{}, ErrUpstream
	}
	return d, nil
}

func (u *HTTPUpstreams) RestaurantOwner(ctx context.Context, id, token string) (string, error) {
	var envelope struct {
		Data struct {
			OwnerID string `json:"owner_id"`
		} `json:"data"`
	}
	status, err := u.get(ctx, u.RestaurantURL, "/api/v1/restaurants/"+url.PathEscape(id), token, &envelope)
	if err != nil || status != 200 || !UUID4(envelope.Data.OwnerID) {
		return "", ErrUpstream
	}
	return envelope.Data.OwnerID, nil
}

func (u *HTTPUpstreams) Rider(ctx context.Context, id, token string) (bool, error) {
	var envelope struct {
		Data struct {
			Role   string `json:"role"`
			Status string `json:"status"`
		} `json:"data"`
	}
	status, err := u.get(ctx, u.UserURL, "/api/v1/users/"+url.PathEscape(id), token, &envelope)
	if err != nil {
		return false, ErrUpstream
	}
	if status == 404 {
		return false, ErrNotFound
	}
	if status != 200 {
		return false, ErrUpstream
	}
	return envelope.Data.Role == "rider" && envelope.Data.Status == "active", nil
}

func (u *HTTPUpstreams) countService(ctx context.Context, base, endpoint, token string, from, to *time.Time) (int64, error) {
	// Existing list APIs publish meta.total_items. Date-filtered counts require
	// created_at on list entries because those APIs do not define date filters.
	if from == nil && to == nil {
		var e struct {
			Meta struct {
				Total *int64 `json:"total_items"`
			} `json:"meta"`
		}
		status, err := u.get(ctx, base, endpoint+"?page=1&limit=1", token, &e)
		if err != nil || status != 200 || e.Meta.Total == nil || *e.Meta.Total < 0 {
			return 0, ErrUpstream
		}
		return *e.Meta.Total, nil
	}
	var count int64
	for page := 1; page <= 10000; page++ {
		var e struct {
			Data []struct {
				CreatedAt string `json:"created_at"`
			} `json:"data"`
			Meta struct {
				TotalPages *int `json:"total_pages"`
			} `json:"meta"`
		}
		status, err := u.get(ctx, base, fmt.Sprintf("%s?page=%d&limit=100", endpoint, page), token, &e)
		if err != nil || status != 200 || e.Meta.TotalPages == nil || *e.Meta.TotalPages < 0 {
			return 0, ErrUpstream
		}
		for _, item := range e.Data {
			t, err := time.Parse(time.RFC3339, item.CreatedAt)
			if err != nil {
				return 0, ErrUpstream
			}
			if (from == nil || !t.Before(*from)) && (to == nil || !t.After(*to)) {
				count++
			}
		}
		if page >= *e.Meta.TotalPages {
			return count, nil
		}
	}
	return 0, ErrUpstream
}

func (u *HTTPUpstreams) DashboardCounts(ctx context.Context, token string, from, to *time.Time) (int64, int64, int64, error) {
	type result struct {
		index int
		n     int64
		err   error
	}
	ch := make(chan result, 3)
	services := []struct{ base, path string }{
		{u.UserURL, "/api/v1/users"},
		{u.RestaurantURL, "/api/v1/restaurants"},
		{u.OrderURL, "/api/v1/orders"},
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i, s := range services {
		go func(i int, base, path string) {
			n, err := u.countService(ctx, base, path, token, from, to)
			ch <- result{i, n, err}
		}(i, s.base, s.path)
	}
	var counts [3]int64
	var firstErr error
	for range services {
		r := <-ch
		if r.err != nil {
			firstErr = r.err
			cancel()
		} else {
			counts[r.index] = r.n
		}
	}
	return counts[0], counts[1], counts[2], firstErr
}
