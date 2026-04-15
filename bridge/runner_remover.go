package bridge

import (
	"context"
	"fmt"

	"github.com/actions/scaleset"
)

// ScaleSetRunnerRemover implements RunnerRemover using a GitHub Actions scaleset client.
type ScaleSetRunnerRemover struct {
	client *scaleset.Client
}

func NewScaleSetRunnerRemover(client *scaleset.Client) *ScaleSetRunnerRemover {
	return &ScaleSetRunnerRemover{client: client}
}

func (r *ScaleSetRunnerRemover) RemoveRunnerByName(ctx context.Context, name string) error {
	runner, err := r.client.GetRunnerByName(ctx, name)
	if err != nil {
		return fmt.Errorf("looking up runner %q: %w", name, err)
	}
	if runner == nil {
		return nil
	}
	return r.client.RemoveRunner(ctx, int64(runner.ID))
}
