package orchard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	orchardclient "github.com/cirruslabs/orchard/pkg/client"
	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"

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

type officialClient struct {
	inner  *orchardclient.Client
	logger *slog.Logger
}

func NewClient(cfg config.OrchardConfig, logger *slog.Logger) (Client, error) {
	opts := []orchardclient.Option{
		orchardclient.WithAddress(cfg.Address),
	}
	if cfg.Username != "" || cfg.Password != "" {
		opts = append(opts, orchardclient.WithCredentials(cfg.Username, cfg.Password))
	}

	inner, err := orchardclient.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("creating orchard client: %w", err)
	}

	if cfg.Insecure {
		logger.Warn("orchard.insecure is deprecated; use an http:// address instead")
	}

	return &officialClient{inner: inner, logger: logger}, nil
}

func (c *officialClient) CreateVM(ctx context.Context, vm *VM) (*VM, error) {
	v1vm := toV1VM(vm)

	if err := c.inner.VMs().Create(ctx, &v1vm); err != nil {
		return nil, fmt.Errorf("creating VM: %w", err)
	}

	// Official Create() returns only error; fetch the created VM for callers
	// that need the response.
	created, err := c.inner.VMs().Get(ctx, vm.Name)
	if err != nil {
		// Creation succeeded; return minimal info if Get fails.
		result := fromV1VM(v1vm)
		return &result, nil
	}
	result := fromV1VM(*created)
	return &result, nil
}

func (c *officialClient) GetVM(ctx context.Context, name string) (*VM, error) {
	vm, err := c.inner.VMs().Get(ctx, name)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("VM %q: %w", name, ErrNotFound)
		}
		return nil, fmt.Errorf("getting VM: %w", err)
	}
	result := fromV1VM(*vm)
	return &result, nil
}

func (c *officialClient) ListVMs(ctx context.Context) ([]VM, error) {
	v1vms, err := c.inner.VMs().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing VMs: %w", err)
	}
	vms := make([]VM, len(v1vms))
	for i, v := range v1vms {
		vms[i] = fromV1VM(v)
	}
	return vms, nil
}

func (c *officialClient) DeleteVM(ctx context.Context, name string) error {
	if err := c.inner.VMs().Delete(ctx, name); err != nil {
		if isNotFound(err) {
			return nil // already gone
		}
		return fmt.Errorf("deleting VM: %w", err)
	}
	return nil
}

func (c *officialClient) ListWorkers(ctx context.Context) ([]Worker, error) {
	v1workers, err := c.inner.Workers().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing workers: %w", err)
	}
	workers := make([]Worker, len(v1workers))
	for i, w := range v1workers {
		workers[i] = fromV1Worker(w)
	}
	return workers, nil
}

func (c *officialClient) Ping(ctx context.Context) error {
	return c.inner.Check(ctx)
}

// --- type conversions ---

func toV1VM(vm *VM) v1.VM {
	result := v1.VM{
		Meta: v1.Meta{
			Name: vm.Name,
		},
		Image:    vm.Image,
		CPU:      vm.CPU,
		Memory:   vm.Memory,
		Headless: true,
		Nested:   vm.Nested,
		Username: "admin",
		Password: "admin",
		VMSpec: v1.VMSpec{
			OS:      v1.OSDarwin,
			Arch:    v1.ArchitectureARM64,
			Runtime: v1.RuntimeTart,
		},
		RestartPolicy: v1.RestartPolicyNever,
	}
	if vm.StartupScript != nil {
		result.StartupScript = &v1.VMScript{
			ScriptContent: vm.StartupScript.ScriptContent,
		}
	}
	// VM labels act as worker-affinity constraints in Orchard — the scheduler
	// only places the VM on workers whose labels are a superset of the VM's.
	if len(vm.Labels) > 0 {
		result.Labels = v1.Labels(vm.Labels)
	}
	return result
}

func fromV1VM(vm v1.VM) VM {
	return VM{
		Name:          vm.Name,
		Image:         vm.Image,
		CPU:           vm.CPU,
		Memory:        vm.Memory,
		Nested:        vm.Nested,
		Status:        mapV1Status(vm.Status),
		StatusMessage: vm.StatusMessage,
		Worker:        vm.Worker,
		StartupScript: fromV1Script(vm.StartupScript),
		Labels:        map[string]string(vm.Labels),
		CreatedAt:     vm.CreatedAt,
	}
}

func mapV1Status(s v1.VMStatus) string {
	switch s {
	case v1.VMStatusPending:
		return VMStatusCreating
	case v1.VMStatusRunning:
		return VMStatusRunning
	case v1.VMStatusFailed:
		return VMStatusFailed
	default:
		return string(s)
	}
}

func fromV1Script(s *v1.VMScript) *VMScript {
	if s == nil {
		return nil
	}
	return &VMScript{ScriptContent: s.ScriptContent}
}

func fromV1Worker(w v1.Worker) Worker {
	return Worker{
		Name:             w.Name,
		Labels:           map[string]string(w.Labels),
		Resources:        map[string]uint64(w.Resources),
		SchedulingPaused: w.SchedulingPaused,
		LastSeen:         w.LastSeen,
	}
}

func isNotFound(err error) bool {
	var apiErr *orchardclient.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}
	return false
}
