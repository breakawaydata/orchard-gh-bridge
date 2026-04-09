package orchard

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"

	"github.com/breakawaydata/orchard-gh-bridge/config"
)

func testClient(t *testing.T, handler http.Handler) Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient(config.OrchardConfig{
		Address:  srv.URL,
		Username: "admin",
		Password: "secret",
	}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreateVM(t *testing.T) {
	var gotName string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			var vm v1.VM
			if err := json.NewDecoder(r.Body).Decode(&vm); err != nil {
				t.Fatal(err)
			}
			gotName = vm.Name
			if !vm.Headless {
				t.Error("expected headless=true")
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/test-vm":
			_ = json.NewEncoder(w).Encode(v1.VM{
				Meta:   v1.Meta{Name: gotName},
				Status: v1.VMStatusPending,
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))

	vm, err := c.CreateVM(context.Background(), &VM{
		Name:  "test-vm",
		Image: "test-image",
		CPU:   4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "test-vm" {
		t.Errorf("sent name = %q, want test-vm", gotName)
	}
	if vm.Status != VMStatusCreating {
		t.Errorf("status = %q, want creating", vm.Status)
	}
}

func TestGetVM(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vms/my-vm" {
			t.Errorf("path = %q, want /v1/vms/my-vm", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(v1.VM{
			Meta:   v1.Meta{Name: "my-vm"},
			Status: v1.VMStatusRunning,
		})
	}))

	vm, err := c.GetVM(context.Background(), "my-vm")
	if err != nil {
		t.Fatal(err)
	}
	if vm.Name != "my-vm" || vm.Status != VMStatusRunning {
		t.Errorf("got %+v", vm)
	}
}

func TestGetVM_NotFound(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	_, err := c.GetVM(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListVMs(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vms" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]v1.VM{
			{Meta: v1.Meta{Name: "vm-1"}, Status: v1.VMStatusRunning},
			{Meta: v1.Meta{Name: "vm-2"}, Status: v1.VMStatusFailed},
		})
	}))

	vms, err := c.ListVMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 {
		t.Fatalf("len = %d, want 2", len(vms))
	}
}

func TestDeleteVM(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))

	if err := c.DeleteVM(context.Background(), "test-vm"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteVM_AlreadyGone(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	if err := c.DeleteVM(context.Background(), "gone-vm"); err != nil {
		t.Errorf("expected no error for 404 delete, got %v", err)
	}
}

func TestListWorkers(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/workers" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]v1.Worker{
			{Meta: v1.Meta{Name: "worker-1"}},
		})
	}))

	workers, err := c.ListWorkers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].Name != "worker-1" {
		t.Errorf("got %+v", workers)
	}
}

func TestAPIError(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "worker at capacity"})
	}))

	_, err := c.CreateVM(context.Background(), &VM{Name: "fail"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got == "" {
		t.Error("empty error message")
	}
}
