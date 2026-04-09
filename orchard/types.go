package orchard

import "time"

type VM struct {
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	CPU           uint64            `json:"cpu"`
	Memory        uint64            `json:"memory"`
	Status        string            `json:"status,omitempty"`
	StatusMessage string            `json:"statusMessage,omitempty"`
	Worker        string            `json:"worker,omitempty"`
	StartupScript *VMScript         `json:"startup_script,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	CreatedAt     time.Time         `json:"createdAt,omitempty"`
}

type VMScript struct {
	ScriptContent string `json:"script_content"`
}

type Worker struct {
	Name             string            `json:"name"`
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
