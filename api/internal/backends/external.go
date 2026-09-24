package backends

import (
	"context"

	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// ExternalBackend represents a run the platform did not start: a training
// script that called mldojo.init() and reports in on its own.
//
// Until this existed a run could only be created by submitting a recipe, so
// "just start logging from the script I already have" -- the way wandb is
// normally used -- was impossible.
type ExternalBackend struct{ rt *Runtime }

func NewExternalBackend(rt *Runtime) *ExternalBackend { return &ExternalBackend{rt: rt} }

func (b *ExternalBackend) Kind() string { return "external" }

// Validate accepts anything: the process is already running, and refusing it
// would only lose the metrics it is trying to report.
func (b *ExternalBackend) Validate(context.Context, *SubmitSpec) error { return nil }

// Submit is a no-op. External runs are created already running, by
// POST /runs/external, not dispatched.
func (b *ExternalBackend) Submit(context.Context, *SubmitSpec) error {
	return Userf("external runs are created by the SDK (mldojo.init()), not submitted")
}

// Cancel records the intent. The process belongs to whoever started it, so
// this marks the run and the SDK stops reporting; nothing is killed.
func (b *ExternalBackend) Cancel(ctx context.Context, run *v1.Run) error {
	if v1.Terminal(run.Status) {
		return nil
	}
	_, err := b.rt.SetStatus(ctx, run.ID, models.RunUpdate{
		Phase:   v1.PhaseCancelled,
		Message: "cancelled; the process is not managed by MLDojo and must be stopped where it runs",
	})
	return err
}

// ArtifactsList returns what the SDK reported; there is no node to rescan.
func (b *ExternalBackend) ArtifactsList(ctx context.Context, run *v1.Run) ([]v1.Artifact, error) {
	return b.rt.Store.ListArtifacts(ctx, run.ID)
}

func (b *ExternalBackend) ArtifactsFetch(context.Context, *v1.Run, string) (string, error) {
	return "", Userf("artifacts of an external run stay on the machine that produced them")
}

func (b *ExternalBackend) GPUStats(context.Context, *v1.Run) ([]v1.GPUStat, error) {
	return nil, nil
}
