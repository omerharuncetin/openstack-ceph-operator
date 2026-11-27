package cephapi

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client talks HTTPS/JSON to the Ceph MGR Dashboard REST API v1.0.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client

	mu    sync.RWMutex
	token string
}

// New creates a Ceph Dashboard REST API client. Call Authenticate explicitly
// or let the first API call authenticate lazily.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("cephapi: BaseURL is required")
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if !strings.HasSuffix(base, "/api") {
		base += "/api"
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &Client{
		baseURL:  base,
		username: cmp.Or(cfg.Username, "admin"),
		password: cfg.Password,
		http: &http.Client{
			Transport: transport,
			Timeout:   20 * time.Second,
		},
	}, nil
}

// Authenticate exchanges username/password for a Dashboard JWT via POST /api/auth.
func (c *Client) Authenticate(ctx context.Context) (string, error) {
	payload := map[string]string{
		"username": c.username,
		"password": c.password,
	}
	var resp AuthResponse
	if err := c.doRaw(ctx, http.MethodPost, "/auth", "", payload, &resp); err != nil {
		return "", err
	}
	if resp.Token == "" {
		return "", errors.New("cephapi: empty token in /api/auth response")
	}
	c.mu.Lock()
	c.token = resp.Token
	c.mu.Unlock()
	return resp.Token, nil
}

// Health queries cluster health, OSD counts, and PG states (GET /api/health/minimal).
func (c *Client) Health(ctx context.Context) (*HealthMinimal, error) {
	var out HealthMinimal
	if err := c.do(ctx, http.MethodGet, "/health/minimal", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListOSDs returns all OSDs registered in the cluster (GET /api/osd).
func (c *Client) ListOSDs(ctx context.Context) ([]OSD, error) {
	var out []OSD
	if err := c.do(ctx, http.MethodGet, "/osd", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListCrushRules returns all CRUSH placement rules (GET /api/crush_rule).
func (c *Client) ListCrushRules(ctx context.Context) ([]CrushRule, error) {
	var out []CrushRule
	if err := c.do(ctx, http.MethodGet, "/crush_rule", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListPools returns all RADOS storage pools (GET /api/pool).
func (c *Client) ListPools(ctx context.Context) ([]Pool, error) {
	var out []Pool
	if err := c.do(ctx, http.MethodGet, "/pool", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetPool fetches a single pool by name (GET /api/pool/{name}).
func (c *Client) GetPool(ctx context.Context, name string) (*Pool, error) {
	var out Pool
	if err := c.do(ctx, http.MethodGet, "/pool/"+url.PathEscape(name), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreatePool creates a new replicated or erasure-coded RADOS pool (POST /api/pool).
func (c *Client) CreatePool(ctx context.Context, req CreatePoolRequest) error {
	req.PoolType = cmp.Or(req.PoolType, "replicated")
	req.PGNum = cmp.Or(req.PGNum, 16)
	if len(req.ApplicationMetadata) == 0 {
		req.ApplicationMetadata = []string{"rbd"}
	}
	return c.do(ctx, http.MethodPost, "/pool", req, nil)
}

// DeletePool deletes a RADOS pool by name (DELETE /api/pool/{name}).
func (c *Client) DeletePool(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/pool/"+url.PathEscape(name), nil, nil)
}

// ListRBDImages lists RBD images, optionally filtered to a single pool.
func (c *Client) ListRBDImages(ctx context.Context, poolName string) ([]RBDImage, error) {
	path := "/block/image"
	if poolName != "" {
		path += "?pool_name=" + url.QueryEscape(poolName)
	}
	var grouped []PoolRBDListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &grouped); err != nil {
		return nil, err
	}
	var out []RBDImage
	for _, grp := range grouped {
		out = append(out, grp.Value...)
	}
	return out, nil
}

// GetRBDImage retrieves details, parent CoW lineage, and snapshots of an RBD image.
func (c *Client) GetRBDImage(ctx context.Context, pool, name string) (*RBDImage, error) {
	var out RBDImage
	if err := c.do(ctx, http.MethodGet, "/block/image/"+imageSpec(pool, name), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateRBDImage provisions a thin-provisioned RBD block device (POST /api/block/image).
func (c *Client) CreateRBDImage(ctx context.Context, req CreateRBDImageRequest) error {
	if len(req.Features) == 0 {
		req.Features = []string{"layering", "exclusive-lock", "object-map", "fast-diff"}
	}
	return c.do(ctx, http.MethodPost, "/block/image", req, nil)
}

// ResizeRBDImage resizes an RBD image online (PUT /api/block/image/{image_spec}).
func (c *Client) ResizeRBDImage(ctx context.Context, pool, name string, sizeBytes int64) error {
	return c.do(ctx, http.MethodPut, "/block/image/"+imageSpec(pool, name), ResizeRBDImageRequest{Size: sizeBytes}, nil)
}

// DeleteRBDImage deletes an RBD image (DELETE /api/block/image/{image_spec}).
func (c *Client) DeleteRBDImage(ctx context.Context, pool, name string) error {
	return c.do(ctx, http.MethodDelete, "/block/image/"+imageSpec(pool, name), nil, nil)
}

// CreateSnapshot creates an immutable RBD snapshot (POST /api/block/image/{image_spec}/snap).
func (c *Client) CreateSnapshot(ctx context.Context, pool, image, snapName string) error {
	path := fmt.Sprintf("/block/image/%s/snap", imageSpec(pool, image))
	return c.do(ctx, http.MethodPost, path, CreateSnapshotRequest{SnapshotName: snapName}, nil)
}

// ProtectSnapshot marks a snapshot protected/unprotected so it can back CoW clones.
func (c *Client) ProtectSnapshot(ctx context.Context, pool, image, snapName string, protected bool) error {
	path := fmt.Sprintf("/block/image/%s/snap/%s", imageSpec(pool, image), url.PathEscape(snapName))
	return c.do(ctx, http.MethodPut, path, ProtectSnapshotRequest{IsProtected: protected}, nil)
}

// DeleteSnapshot deletes an unprotected RBD snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, pool, image, snapName string) error {
	path := fmt.Sprintf("/block/image/%s/snap/%s", imageSpec(pool, image), url.PathEscape(snapName))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// CloneSnapshot creates a CoW RBD clone from a protected snapshot.
func (c *Client) CloneSnapshot(ctx context.Context, srcPool, srcImage, snapName string, req CloneSnapshotRequest) error {
	if len(req.Features) == 0 {
		req.Features = []string{"layering", "exclusive-lock", "object-map", "fast-diff"}
	}
	path := fmt.Sprintf("/block/image/%s/snap/%s/clone", imageSpec(srcPool, srcImage), url.PathEscape(snapName))
	return c.do(ctx, http.MethodPost, path, req, nil)
}

func imageSpec(pool, name string) string {
	return url.PathEscape(pool + "/" + name)
}

func (c *Client) getToken(ctx context.Context) (string, error) {
	c.mu.RLock()
	tok := c.token
	c.mu.RUnlock()
	if tok != "" {
		return tok, nil
	}
	return c.Authenticate(ctx)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := c.getToken(ctx)
	if err != nil {
		return err
	}
	err = c.doRaw(ctx, method, path, tok, body, out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		// Re-authenticate once on 401 and retry.
		tok, authErr := c.Authenticate(ctx)
		if authErr != nil {
			return authErr
		}
		return c.doRaw(ctx, method, path, tok, body, out)
	}
	return err
}

func (c *Client) doRaw(ctx context.Context, method, path, token string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("cephapi marshal %s %s: %w", method, path, err)
		}
		reqBody = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", AcceptHeader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			Detail:     strings.TrimSpace(string(respBytes)),
		}
	}

	if out != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return fmt.Errorf("cephapi decode %s %s: %w (body=%s)", method, path, err, string(respBytes))
		}
	}
	return nil
}

// IsNotFound reports whether err represents a 404 Not Found from the Ceph API.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}
