package core

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
)

// Rerun re-submits a past run. Everything needed was already recorded --
// the resolved command, params, env, resources, datasets and the code bundle
// or patch blob -- there was simply no way to ask for it back.
//
// The recipe is rebuilt from the run rather than read from the experiment,
// so a run reproduces as it was even if the experiment's recipe has since
// changed. target and params override what the run used.
func (s *Service) Rerun(ctx context.Context, id, target string, params map[string]any) (*v1.SubmitResponse, error) {
	run, err := s.store().GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	md := run.Metadata
	if md.Cmd == "" {
		return nil, models.Conflict("run %s has no recorded command to rerun", run.ID)
	}
	rec := recipes.Recipe{
		APIVersion: "mldojo/v1",
		Kind:       "Experiment",
		Metadata: recipes.Metadata{
			Project: run.Project, Name: run.Experiment,
			Description: fmt.Sprintf("rerun of %s", run.ID), Tags: md.Tags,
		},
		Code: recipes.Code{Source: md.CodeSource, Repo: md.CodeRepo, Ref: md.CodeRef},
		Run: recipes.RunBlock{
			Cmd: md.Cmd, Workdir: md.Workdir, Setup: md.Setup,
			Params: md.Params, Env: md.ExtraEnv, Wandb: md.Wandb,
		},
	}
	// env and resources were stored resolved; feed them back as the defaults
	// so the new run gets exactly the same shape.
	if err := jsonInto(run.Env, &rec.Env.Default); err != nil {
		return nil, err
	}
	if err := jsonInto(run.Resources, &rec.Resources.Default); err != nil {
		return nil, err
	}
	if md.Outputs != nil {
		rec.Outputs = recipes.OutputsBlock{
			Logs: md.Outputs.Logs, Checkpoints: md.Outputs.Checkpoints,
			Videos: md.Outputs.Videos, Images: md.Outputs.Images, Metrics: md.Outputs.Metrics,
			Episodes: md.Outputs.Episodes, Model: md.Outputs.Model,
		}
	}
	// A model reference is replayed as a reference, pinned to the version
	// this run read: the rerun runs against the same bytes and still says
	// what it was. Its resolved path is already in params; drop it, or the
	// two would collide.
	for _, m := range md.Models {
		ref := recipes.ModelRef{Name: m.Name, Version: strconv.Itoa(m.Version), As: m.As}
		rec.Models = append(rec.Models, ref)
		if rec.Run.Params != nil {
			p := maps.Clone(rec.Run.Params)
			delete(p, ref.Param())
			rec.Run.Params = p
		}
	}
	for _, d := range md.Datasets {
		rec.Datasets = append(rec.Datasets, recipes.DatasetRef{Name: d.Name, Version: d.Version, Mount: d.Mount})
	}
	y, err := yaml.Marshal(rec)
	if err != nil {
		return nil, err
	}
	if target == "" {
		target = run.Target
	}
	req := v1.SubmitRequest{
		RecipeYAML: string(y),
		Target:     target,
		Params:     params,
		Notes:      fmt.Sprintf("rerun of %s", run.ID),
		Code: &v1.CodeInfo{
			Source: md.CodeSource, Repo: md.CodeRepo, Ref: md.CodeRef,
			Commit: run.CodeCommit, Dirty: md.CodeDirty,
			PatchURI: run.CodePatchURI, BundleURI: md.CodeBundleURI,
		},
	}
	return s.Submit(ctx, req)
}

// jsonInto decodes stored JSON into dst, treating empty as absent.
func jsonInto(raw json.RawMessage, dst *map[string]any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dst)
}
