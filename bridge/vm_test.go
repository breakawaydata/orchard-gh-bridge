package bridge

import (
	"strings"
	"testing"
)

func TestVMName(t *testing.T) {
	name := VMName("macos-sequoia-xcode-16.3")
	if !strings.HasPrefix(name, "gha-orchard-macos-sequoia-xcode-16-3-") {
		t.Errorf("unexpected name: %s", name)
	}
	if len(name) > 63 {
		t.Errorf("name too long: %d chars", len(name))
	}
}

func TestVMName_LongName(t *testing.T) {
	long := strings.Repeat("a", 100)
	name := VMName(long)
	if len(name) > 63 {
		t.Errorf("name too long: %d chars", len(name))
	}
	if !strings.HasPrefix(name, "gha-orchard-") {
		t.Errorf("missing prefix: %s", name)
	}
}

func TestVMName_Unique(t *testing.T) {
	a := VMName("test")
	b := VMName("test")
	if a == b {
		t.Errorf("names should be unique: %s == %s", a, b)
	}
}

func TestStartupScript(t *testing.T) {
	script := StartupScript("abc123encoded", 0)
	if !strings.Contains(script, "abc123encoded") {
		t.Error("script missing JIT config")
	}
	if !strings.Contains(script, "ACTIONS_RUNNER_INPUT_JITCONFIG") {
		t.Error("script missing env var")
	}
	if !strings.HasPrefix(script, "#!/bin/bash") {
		t.Error("script missing shebang")
	}
	if strings.Contains(script, "brew install") {
		t.Error("script without dockerPort should not install anything")
	}
}

func TestStartupScript_DockerPort(t *testing.T) {
	script := StartupScript("abc123encoded", 2375)
	if !strings.Contains(script, "DOCKER_HOST_IP=$(route -n get default") {
		t.Error("script should auto-detect host gateway")
	}
	if !strings.Contains(script, `:2375"`) {
		t.Error("script should use configured port")
	}
	if !strings.Contains(script, "brew install docker") {
		t.Error("script should install docker CLI")
	}
	if !strings.Contains(script, "< /dev/null") {
		t.Error("brew install must redirect stdin")
	}
}
