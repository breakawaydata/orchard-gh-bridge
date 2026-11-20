package bridge

import (
	"slices"
	"testing"

	"github.com/breakawaydata/orchard-gh-bridge/config"
)

func TestVMSpec_SoftnetSet(t *testing.T) {
	b := New(Config{
		ScaleSetName: "test",
		VMConfig: config.VMConfig{
			Image:           "img:1",
			NetSoftnet:      true,
			NetSoftnetAllow: []string{"10.0.0.0/8"},
			NetSoftnetBlock: []string{"0.0.0.0/0"},
		},
		Logger: testLogger(),
	})
	vm := b.vmSpec("gha-orchard-test-aaaaaaaa", "script", 4, 8192, nil)
	if !vm.NetSoftnet {
		t.Error("NetSoftnet = false, want true")
	}
	if !slices.Equal(vm.NetSoftnetAllow, []string{"10.0.0.0/8"}) {
		t.Errorf("NetSoftnetAllow = %v", vm.NetSoftnetAllow)
	}
	if !slices.Equal(vm.NetSoftnetBlock, []string{"0.0.0.0/0"}) {
		t.Errorf("NetSoftnetBlock = %v", vm.NetSoftnetBlock)
	}
}

func TestVMSpec_SoftnetUnset(t *testing.T) {
	b := New(Config{
		ScaleSetName: "test",
		VMConfig:     config.VMConfig{Image: "img:1"},
		Logger:       testLogger(),
	})
	vm := b.vmSpec("gha-orchard-test-aaaaaaaa", "script", 4, 8192, nil)
	if vm.NetSoftnet || vm.NetSoftnetAllow != nil || vm.NetSoftnetBlock != nil {
		t.Errorf("softnet must stay off when unset: %+v", vm)
	}
}
