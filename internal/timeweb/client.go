package timeweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	DefaultBaseURL   = "https://api.timeweb.cloud"
	requestTimeout   = 30 * time.Second
	pageSize         = 100
	maxPages         = 10000
	maxResponseBytes = 32 << 20
)

var (
	ErrNotFound                   = errors.New("timeweb resource not found")
	ErrDeleteConfirmationRequired = errors.New("timeweb deletion requires confirmation; use an API token with is_able_to_delete enabled for unattended cleanup")
	ErrDeletionPending            = errors.New("timeweb deletion is pending; reconcile the server before considering it deleted")
)

// APIError intentionally contains no server-supplied message or response body:
// Timeweb responses may contain passwords or echo the cloud-init registration
// token. StatusCode remains available for retries and diagnostics.
type APIError struct {
	Operation  string
	StatusCode int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("timeweb %s: HTTP %d", e.Operation, e.StatusCode)
}

func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.StatusCode == http.StatusNotFound
}

// Client is safe for concurrent use. It never follows redirects or retries
// mutations: replaying a successful but interrupted create could leak a VM.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient uses Timeweb's API by default. A custom base URL can contain a path
// prefix; a final /api/v1 is also accepted. Plain HTTP is restricted to localhost
// and numeric loopback addresses, for tests and local proxies.
func NewClient(baseURL, token string, httpClient *http.Client) (*Client, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("timeweb: invalid API base URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		isLoopback := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
		if u.Scheme != "http" || !isLoopback {
			return nil, errors.New("timeweb: API base URL requires HTTPS")
		}
	}
	if strings.ContainsAny(u.Path, "\\") || u.RawPath != "" {
		return nil, errors.New("timeweb: invalid API base URL path")
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.IndexFunc(token, unicode.IsSpace) >= 0 || strings.IndexFunc(token, unicode.IsControl) >= 0 {
		return nil, errors.New("timeweb: API token must be nonempty and contain no whitespace")
	}
	client := &http.Client{Timeout: requestTimeout}
	if httpClient != nil {
		clone := *httpClient
		client = &clone
		if client.Timeout <= 0 {
			client.Timeout = requestTimeout
		}
	}
	// Go normally forwards Authorization to subdomains on redirect. Reject every
	// redirect, including same-origin redirects and redirects from custom clients.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api/v1")
	return &Client{baseURL: strings.TrimRight(u.String(), "/"), token: token, httpClient: client}, nil
}

// ListServers traverses every offset page. Metadata totals take precedence over
// short pages because an API-side page cap need not match the requested limit.
func (c *Client) ListServers(ctx context.Context) ([]Server, error) {
	servers := make([]Server, 0)
	seen := make(map[int64]struct{})
	offset := 0
	for page := 0; page < maxPages; page++ {
		var response struct {
			Servers []Server `json:"servers"`
			Meta    struct {
				Total *int64 `json:"total"`
			} `json:"meta"`
		}
		path := "/servers?limit=" + strconv.Itoa(pageSize) + "&offset=" + strconv.Itoa(offset)
		status, body, err := c.request(ctx, "list servers", http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, unexpectedStatus("list servers", status)
		}
		if err := decodeResponse("list servers", body, &response); err != nil {
			return nil, err
		}
		if response.Servers == nil {
			return nil, invalidResponse("list servers")
		}
		if response.Meta.Total != nil && (*response.Meta.Total < 0 || *response.Meta.Total < int64(offset+len(response.Servers))) {
			return nil, invalidResponse("list servers pagination")
		}
		if len(response.Servers) == 0 {
			if response.Meta.Total != nil && int64(offset) < *response.Meta.Total {
				return nil, invalidResponse("list servers pagination")
			}
			return servers, nil
		}
		added := false
		for _, server := range response.Servers {
			if server.ID <= 0 {
				return nil, invalidResponse("list servers")
			}
			if _, exists := seen[server.ID]; exists {
				continue
			}
			seen[server.ID] = struct{}{}
			servers = append(servers, server)
			added = true
		}
		if !added {
			return nil, invalidResponse("list servers pagination made no progress")
		}
		offset += len(response.Servers)
		if response.Meta.Total != nil && int64(offset) >= *response.Meta.Total {
			if int64(len(servers)) != *response.Meta.Total {
				return nil, invalidResponse("list servers pagination changed")
			}
			return servers, nil
		}
	}
	return nil, errors.New("timeweb list servers: pagination limit exceeded")
}

func (c *Client) GetServer(ctx context.Context, id int64) (Server, error) {
	if id <= 0 {
		return Server{}, errors.New("timeweb get server: ID must be positive")
	}
	status, body, err := c.request(ctx, "get server", http.MethodGet, serverPath(id), nil)
	if err != nil {
		return Server{}, err
	}
	if status != http.StatusOK {
		return Server{}, unexpectedStatus("get server", status)
	}
	server, err := decodeServer("get server", body)
	if err == nil && server.ID != id {
		return Server{}, invalidResponse("get server ID mismatch")
	}
	return server, err
}

func (c *Client) CreateServer(ctx context.Context, req CreateServerRequest) (Server, error) {
	if req.Name == "" || req.PresetID <= 0 || (req.OSID > 0) == (req.ImageID != "") || req.OSID < 0 {
		return Server{}, errors.New("timeweb create server: name, positive preset, and exactly one OS or image are required")
	}
	status, body, err := c.request(ctx, "create server", http.MethodPost, "/servers", req)
	if err != nil {
		return Server{}, err
	}
	if status != http.StatusCreated {
		return Server{}, unexpectedStatus("create server", status)
	}
	return decodeServer("create server", body)
}

// DeleteServer succeeds when the API accepts an immediate deletion or a move
// into quarantine. A deletion requiring interactive confirmation is an error,
// even when Timeweb returns HTTP 200. HTTP 404 is idempotent success.
func (c *Client) DeleteServer(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("timeweb delete server: ID must be positive")
	}
	status, body, err := c.request(ctx, "delete server", http.MethodDelete, serverPath(id), nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == http.StatusNoContent {
		return nil
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return unexpectedStatus("delete server", status)
	}
	if status == http.StatusAccepted && len(bytes.TrimSpace(body)) == 0 {
		return ErrDeletionPending
	}
	var response struct {
		Delete *struct {
			Hash  string `json:"hash"`
			Moved *bool  `json:"is_moved_in_quarantine"`
		} `json:"server_delete"`
	}
	if err := decodeResponse("delete server", body, &response); err != nil {
		return err
	}
	if response.Delete == nil {
		return invalidResponse("delete server")
	}
	if response.Delete.Hash != "" {
		return ErrDeleteConfirmationRequired
	}
	if status == http.StatusAccepted {
		return ErrDeletionPending
	}
	if response.Delete.Moved == nil {
		return invalidResponse("delete server")
	}
	return nil
}

// PerformAction uses the documented action endpoint. Only power operations are
// accepted: image installation, password resets, cloning, and deletion have
// different lifecycle and confirmation semantics.
func (c *Client) PerformAction(ctx context.Context, id int64, action string) error {
	if id <= 0 {
		return errors.New("timeweb perform action: ID must be positive")
	}
	switch action {
	case "start", "shutdown", "reboot", "hard_shutdown", "hard_reboot":
	default:
		return errors.New("timeweb perform action: unsupported power action")
	}
	status, _, err := c.request(ctx, "perform action", http.MethodPost, serverPath(id)+"/action", struct {
		Action string `json:"action"`
	}{action})
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return unexpectedStatus("perform action", status)
	}
	return nil
}

// SetNATMode changes legacy OVN routing. SNAT is not available for BGP networks;
// BGP networking must use a separately configured router and guest routes.
func (c *Client) SetNATMode(ctx context.Context, id int64, mode string) error {
	if id <= 0 {
		return errors.New("timeweb set NAT mode: ID must be positive")
	}
	switch mode {
	case "snat", "no_nat", "dnat_and_snat":
	default:
		return errors.New("timeweb set NAT mode: unsupported mode")
	}
	status, _, err := c.request(ctx, "set NAT mode", http.MethodPatch, serverPath(id)+"/local-networks/nat-mode", struct {
		Mode string `json:"nat_mode"`
	}{mode})
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return unexpectedStatus("set NAT mode", status)
	}
	return nil
}

func serverPath(id int64) string { return "/servers/" + strconv.FormatInt(id, 10) }

func (c *Client) request(ctx context.Context, operation, method, path string, payload any) (int, []byte, error) {
	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			return 0, nil, errors.New("timeweb: unable to encode request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, errors.New("timeweb: unable to construct request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "garm-provider-timeweb")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, safeRequestError(ctx, operation, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Do not decode or retain error responses. Even syntactically valid error
		// codes and request IDs could echo secrets from the request.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return response.StatusCode, nil, &APIError{Operation: operation, StatusCode: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return response.StatusCode, nil, safeRequestError(ctx, operation, err)
	}
	if len(body) > maxResponseBytes {
		return response.StatusCode, nil, errors.New("timeweb: response exceeds size limit")
	}
	return response.StatusCode, body, nil
}

func safeRequestError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("timeweb %s: %w", operation, ctx.Err())
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return fmt.Errorf("timeweb %s: %w", operation, context.DeadlineExceeded)
	}
	// A transport error can include a URL, arbitrary proxy output, or request
	// contents. Preserve the category, never the untrusted error text.
	return fmt.Errorf("timeweb %s: request failed", operation)
}

func unexpectedStatus(operation string, status int) error {
	return &APIError{Operation: operation, StatusCode: status}
}
func invalidResponse(operation string) error {
	return fmt.Errorf("timeweb %s: invalid API response", operation)
}

func decodeResponse(operation string, body []byte, target any) error {
	if err := json.Unmarshal(body, target); err != nil {
		return invalidResponse(operation)
	}
	return nil
}

func decodeServer(operation string, body []byte) (Server, error) {
	var response struct {
		Server *Server `json:"server"`
	}
	if err := decodeResponse(operation, body, &response); err != nil {
		return Server{}, err
	}
	if response.Server == nil || response.Server.ID <= 0 {
		return Server{}, invalidResponse(operation)
	}
	return *response.Server, nil
}
