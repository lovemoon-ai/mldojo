package core

import (
	"context"
	"strconv"

	"github.com/lovemoon-ai/mldojo/api/internal/backends"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
)

// resolveModels turns a recipe's models: block into run parameters.
//
// The registry existed before this and went unused: the first real evaluation
// pasted a checkpoint path, because pasting is cheaper than registering. So
// the reference has to be the cheap thing. `models: [{name: policy, as: ckpt}]`
// keeps the command writing ${ckpt} and costs one line, and in exchange the
// run records what it actually read.
func (s *Service) resolveModels(ctx context.Context, r *recipes.Recipe, t recipes.Target) ([]v1.RunModel, error) {
	var out []v1.RunModel
	for _, ref := range r.Models {
		proj, name := ref.Project(r.Metadata.Project)
		m, err := s.store().GetModel(ctx, proj, name)
		if models.IsNotFound(err) {
			return nil, backends.Userf("model %s/%s is not registered (see `mldojo model ls`)", proj, name)
		} else if err != nil {
			return nil, err
		}
		vs, err := s.store().ListModelVersions(ctx, m.ID) // newest first
		if err != nil {
			return nil, err
		}
		v := pickVersion(vs, ref.Version)
		if v == nil {
			want := ref.Version
			if want == "" {
				want = "latest"
			}
			return nil, backends.Userf("model %s/%s has no version %s (%d registered)", proj, name, want, len(vs))
		}
		uri := v.URI
		if node, path, isNode := backends.ParseNodeURI(uri); isNode {
			// The artifact was never copied anywhere: it is a path on the
			// node that produced it. Saying so beats handing the job a path
			// that does not exist where it runs.
			if t.Kind == "node" && t.ID != node {
				return nil, backends.Userf("model %s/%s@%d is %s on node %s, which node:%s cannot read",
					proj, name, v.Version, path, node, t.ID)
			}
			uri = path
		}
		if r.Run.Params == nil {
			r.Run.Params = map[string]any{}
		}
		r.Run.Params[ref.Param()] = uri
		out = append(out, v1.RunModel{Name: proj + "/" + name, Version: v.Version, URI: uri, As: ref.Param()})
	}
	return out, nil
}

// pickVersion resolves "", latest, production, staging or a version number
// against the versions of one model, newest first.
func pickVersion(vs []models.ModelVersion, want string) *models.ModelVersion {
	switch want {
	case "", "latest":
		if len(vs) > 0 {
			return &vs[0]
		}
	case models.StageProduction, models.StageStaging:
		for i := range vs {
			if vs[i].Stage == want {
				return &vs[i]
			}
		}
	default:
		n, err := strconv.Atoi(want)
		if err != nil {
			return nil
		}
		for i := range vs {
			if vs[i].Version == n {
				return &vs[i]
			}
		}
	}
	return nil
}
