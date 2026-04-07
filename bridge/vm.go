package bridge

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

var nonAlphanumHyphen = regexp.MustCompile(`[^a-z0-9-]`)

// VMName generates a unique, k8s-label-safe VM name.
// Format: gha-{scaleSetName}-{shortUUID}, max 63 chars.
func VMName(scaleSetName string) string {
	sanitized := strings.ToLower(scaleSetName)
	sanitized = nonAlphanumHyphen.ReplaceAllString(sanitized, "-")
	sanitized = strings.Trim(sanitized, "-")

	short := uuid.New().String()[:8]
	name := fmt.Sprintf("gha-%s-%s", sanitized, short)

	if len(name) > 63 {
		name = name[:63]
		name = strings.TrimRight(name, "-")
	}
	return name
}

// StartupScript generates the bash script injected into the Orchard VM.
// The script configures the GitHub Actions runner with the JIT config and runs it.
func StartupScript(jitConfig string) string {
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

export ACTIONS_RUNNER_INPUT_JITCONFIG="%s"

# Use runner pre-installed in the Tart image if available
if [ -d /opt/runner ]; then
  cd /opt/runner
  ./run.sh
  exit $?
fi

# Fallback: download and install runner
RUNNER_VERSION="2.323.0"
RUNNER_ARCH="osx-arm64"
RUNNER_DIR="$HOME/actions-runner"

mkdir -p "$RUNNER_DIR" && cd "$RUNNER_DIR"
curl -sL -o runner.tar.gz \
  "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"
tar xzf runner.tar.gz
rm runner.tar.gz

./run.sh
`, jitConfig)
}
