package orchard

import "time"

type VM struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	CPU    uint64 `json:"cpu"`
	Memory uint64 `json:"memory"`
	Nested bool   `json:"nested,omitempty"`
	// DiskSize resizes the VM's disk to this many GB after clone (Orchard
	// runs `tart set --disk-size`). Zero is omitted, so the image's own disk
	// size is kept.
	DiskSize uint64 `json:"diskSize,omitempty"`
	// NetSoftnet, NetSoftnetAllow and NetSoftnetBlock map to Orchard's
	// Softnet fields; Orchard passes them to `tart run --net-softnet*`.
	NetSoftnet      bool              `json:"netSoftnet,omitempty"`
	NetSoftnetAllow []string          `json:"netSoftnetAllow,omitempty"`
	NetSoftnetBlock []string          `json:"netSoftnetBlock,omitempty"`
	Status          string            `json:"status,omitempty"`
	StatusMessage   string            `json:"statusMessage,omitempty"`
	Worker          string            `json:"worker,omitempty"`
	StartupScript   *VMScript         `json:"startup_script,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	CreatedAt       time.Time         `json:"createdAt,omitempty"`
}

type VMScript struct {
	ScriptContent string `json:"script_content"`
}

type Worker struct {
	Name             string            `json:"name"`
	Labels           map[string]string `json:"labels,omitempty"`
	Resources        map[string]uint64 `json:"resources,omitempty"`
	SchedulingPaused bool              `json:"scheduling_paused"`
	LastSeen         time.Time         `json:"last_seen"`
}

const (
	VMStatusCreating = "creating"
	VMStatusRunning  = "running"
	VMStatusStopped  = "stopped"
	VMStatusFailed   = "failed"
)
