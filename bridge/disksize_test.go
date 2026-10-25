package bridge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/breakawaydata/orchard-gh-bridge/config"
)

func TestVMSpec_DiskSizeSet(t *testing.T) {
	b := New(Config{
		ScaleSetName: "test",
		VMConfig:     config.VMConfig{Image: "img:1", DiskSize: 200},
		Logger:       testLogger(),
	})
	vm := b.vmSpec("gha-orchard-test-aaaaaaaa", "script", 6, 12288, map[string]string{"k": "v"})
	if vm.DiskSize != 200 {
		t.Errorf("DiskSize = %d, want 200", vm.DiskSize)
	}
	if vm.Image != "img:1" || vm.CPU != 6 || vm.Memory != 12288 || vm.Labels["k"] != "v" {
		t.Errorf("unexpected spec: %+v", vm)
	}
	if vm.StartupScript == nil || vm.StartupScript.ScriptContent != "script" {
		t.Errorf("startup script not carried: %+v", vm.StartupScript)
	}
	body, err := json.Marshal(vm)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"diskSize":200`) {
		t.Errorf("request body missing diskSize: %s", body)
	}
}

func TestVMSpec_DiskSizeOmittedLeavesRequestUnchanged(t *testing.T) {
	b := New(Config{
		ScaleSetName: "test",
		VMConfig:     config.VMConfig{Image: "img:1"},
		Logger:       testLogger(),
	})
	vm := b.vmSpec("gha-orchard-test-aaaaaaaa", "script", 4, 8192, nil)
	if vm.DiskSize != 0 {
		t.Errorf("DiskSize = %d, want 0", vm.DiskSize)
	}
	body, err := json.Marshal(vm)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "diskSize") {
		t.Errorf("diskSize must be omitted when unset so Orchard keeps the image size: %s", body)
	}
}
