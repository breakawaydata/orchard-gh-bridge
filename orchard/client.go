package orchard

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/config"
)

var ErrNotFound = errors.New("not found")

type Client interface {
	CreateVM(ctx context.Context, vm *VM) (*VM, error)
	GetVM(ctx context.Context, name string) (*VM, error)
	ListVMs(ctx context.Context) ([]VM, error)
	DeleteVM(ctx context.Context, name string) error
	ListWorkers(ctx context.Context) ([]Worker, error)
	Ping(ctx context.Context) error
}

type httpClient struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
	logger     *slog.Logger
}

func NewClient(cfg config.OrchardConfig, logger *slog.Logger) Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &httpClient{
		baseURL:  cfg.Address,
		username: cfg.Username,
		password: cfg.Password,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
		logger: logger,
	}
}

func (c *httpClient) CreateVM(ctx context.Context, vm *VM) (*VM, error) {
	body, err := json.Marshal(vm)
	if err != nil {
		return nil, fmt.Errorf("marshaling VM: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost, "/v1/vms", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating VM: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, c.parseError(resp, "creating VM")
	}

	var created VM
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, fmt.Errorf("decoding create VM response: %w", err)
	}
	return &created, nil
}

func (c *httpClient) GetVM(ctx context.Context, name string) (*VM, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/vms/"+name, nil)
	if err != nil {
		return nil, fmt.Errorf("getting VM: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("VM %q: %w", name, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp, "getting VM")
	}

	var vm VM
	if err := json.NewDecoder(resp.Body).Decode(&vm); err != nil {
		return nil, fmt.Errorf("decoding VM response: %w", err)
	}
	return &vm, nil
}

func (c *httpClient) ListVMs(ctx context.Context) ([]VM, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/vms", nil)
	if err != nil {
		return nil, fmt.Errorf("listing VMs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp, "listing VMs")
	}

	var vms []VM
	if err := json.NewDecoder(resp.Body).Decode(&vms); err != nil {
		return nil, fmt.Errorf("decoding VMs response: %w", err)
	}
	return vms, nil
}

func (c *httpClient) DeleteVM(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/vms/"+name, nil)
	if err != nil {
		return fmt.Errorf("deleting VM: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil // already gone
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return c.parseError(resp, "deleting VM")
	}
	return nil
}

func (c *httpClient) ListWorkers(ctx context.Context) ([]Worker, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/workers", nil)
	if err != nil {
		return nil, fmt.Errorf("listing workers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp, "listing workers")
	}

	var workers []Worker
	if err := json.NewDecoder(resp.Body).Decode(&workers); err != nil {
		return nil, fmt.Errorf("decoding workers response: %w", err)
	}
	return workers, nil
}

func (c *httpClient) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/v1/workers", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("orchard ping: status %d", resp.StatusCode)
	}
	return nil
}

func (c *httpClient) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	return c.httpClient.Do(req)
}

func (c *httpClient) parseError(resp *http.Response, context string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var apiErr apiError
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
		return fmt.Errorf("%s: %s (status %d)", context, apiErr.Message, resp.StatusCode)
	}
	return fmt.Errorf("%s: status %d: %s", context, resp.StatusCode, string(body))
}
