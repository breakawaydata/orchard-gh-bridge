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
	script := StartupScript("abc123encoded", false)
	if !strings.Contains(script, "abc123encoded") {
		t.Error("script missing JIT config")
	}
	if !strings.Contains(script, "ACTIONS_RUNNER_INPUT_JITCONFIG") {
		t.Error("script missing env var")
	}
	if !strings.HasPrefix(script, "#!/bin/bash") {
		t.Error("script missing shebang")
	}
	if strings.Contains(script, "colima") {
		t.Error("non-nested script should not install Docker")
	}
}

func TestStartupScript_Nested(t *testing.T) {
	script := StartupScript("abc123encoded", true)
	if !strings.Contains(script, "colima") {
		t.Error("nested script should install Docker via Colima")
	}
	if !strings.Contains(script, "docker-buildx") {
		t.Error("nested script should install docker-buildx")
	}
}
