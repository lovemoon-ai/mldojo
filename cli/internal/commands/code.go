package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lovemoon-ai/mldojo/cli/internal/client"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
)

// maxBundle guards against accidentally shipping datasets/checkpoints.
const maxBundle = 2 << 30

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// gitRoot returns the repository root containing dir, or "".
func gitRoot(dir string) string {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// dirtyPatch returns HEAD..worktree as a binary patch including untracked
// files, without touching the real index.
func dirtyPatch(root string) (string, error) {
	gitDir, err := git(root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	idx, err := os.CreateTemp("", "mldojo-index-*")
	if err != nil {
		return "", err
	}
	idx.Close()
	defer os.Remove(idx.Name())
	if b, err := os.ReadFile(filepath.Join(strings.TrimSpace(gitDir), "index")); err == nil {
		os.WriteFile(idx.Name(), b, 0o600)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+idx.Name())
		out, err := cmd.Output()
		return string(out), err
	}
	if _, err := run("add", "-A"); err != nil {
		return "", fmt.Errorf("git add (temp index): %w", err)
	}
	return run("diff", "--cached", "--binary", "HEAD")
}

// listFiles returns files to bundle, relative to dir.
func listFiles(dir string, inGit bool) ([]string, error) {
	if inGit {
		out, err := git(dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", ".")
		if err != nil {
			return nil, err
		}
		var files []string
		for _, f := range strings.Split(out, "\x00") {
			if f != "" {
				files = append(files, f)
			}
		}
		return files, nil
	}
	skip := map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true,
		".mypy_cache": true, ".pytest_cache": true, "wandb": true, "outputs": true, ".mldojo": true}
	ignore := readIgnore(filepath.Join(dir, ".mldojoignore"))
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skip[d.Name()] || ignored(ignore, rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !ignored(ignore, rel) {
			files = append(files, rel)
		}
		return nil
	})
	return files, err
}

func readIgnore(p string) []string {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var pats []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			pats = append(pats, strings.TrimSuffix(l, "/"))
		}
	}
	return pats
}

func ignored(pats []string, rel string) bool {
	base := filepath.Base(rel)
	for _, p := range pats {
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
	}
	return false
}

// tarGz bundles files (relative to dir) into a gzipped tar.
func tarGz(dir string, files []string) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		st, err := os.Lstat(p)
		if err != nil {
			continue // deleted in worktree
		}
		if st.IsDir() {
			continue // submodule gitlinks
		}
		link := ""
		if st.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}
		h, err := tar.FileInfoHeader(st, link)
		if err != nil {
			continue
		}
		h.Name = f
		h.Uname, h.Gname = "", ""
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if st.Mode().IsRegular() {
			fh, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			_, err = io.Copy(tw, fh)
			fh.Close()
			if err != nil {
				return nil, err
			}
		}
		if buf.Len() > maxBundle {
			return nil, fmt.Errorf("code bundle exceeds %d bytes; add large paths to .gitignore/.mldojoignore", maxBundle)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// prepareCode bundles the code for a recipe and uploads bundle + patch.
// recipeDir anchors relative code.path values.
func prepareCode(ctx context.Context, cl *client.Client, r *recipes.Recipe, recipeDir string, logf func(string, ...any)) (*v1.CodeInfo, error) {
	ci := &v1.CodeInfo{Source: r.Code.Source, Repo: r.Code.Repo, Ref: r.Code.Ref}
	switch r.Code.Source {
	case "none":
		// The project already lives on the node. Shipping it is not just
		// wasteful here, it is impossible: the code sits next to its base
		// weights, its virtualenvs and its simulator assets.
		logf("code: none (running in place on the node)")
		return ci, nil
	case "inline-patch":
		if r.Code.Patch != "" {
			b, err := os.ReadFile(filepath.Join(recipeDir, r.Code.Patch))
			if err != nil {
				return nil, usageErr("code.patch: %v", err)
			}
			ref, err := cl.UploadBlob(ctx, bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			ci.PatchURI, ci.Dirty = ref.URI, true
		}
		return ci, nil
	}
	dir := r.Code.Path
	if dir == "" {
		dir = "."
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(recipeDir, dir)
	}
	dir, _ = filepath.Abs(dir)
	root := gitRoot(dir)
	if r.Code.Source == "git" {
		if root == "" {
			if r.Code.Repo == "" {
				return nil, usageErr("code.source git: %s is not a git checkout and code.repo is empty", dir)
			}
			logf("code: agent will clone %s@%s", r.Code.Repo, r.Code.Ref)
			return ci, nil
		}
		dir = root // bundle the whole repository; run.workdir is relative to its root
	}
	bundleDir := dir
	if root != "" {
		head, err := git(root, "rev-parse", "HEAD")
		if err == nil {
			ci.Commit = strings.TrimSpace(head)
			patch, err := dirtyPatch(root)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(patch) != "" {
				ci.Dirty = true
				ref, err := cl.UploadBlob(ctx, strings.NewReader(patch))
				if err != nil {
					return nil, err
				}
				ci.PatchURI = ref.URI
			}
			if ci.Repo == "" {
				if u, err := git(root, "remote", "get-url", "origin"); err == nil {
					ci.Repo = strings.TrimSpace(u)
				}
			}
			if ci.Ref == "" {
				if b, err := git(root, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
					ci.Ref = strings.TrimSpace(b)
				}
			}
		}
	}
	files, err := listFiles(bundleDir, root != "")
	if err != nil {
		return nil, err
	}
	buf, err := tarGz(bundleDir, files)
	if err != nil {
		return nil, err
	}
	ref, err := cl.UploadBlob(ctx, buf)
	if err != nil {
		return nil, err
	}
	ci.BundleURI = ref.URI
	dirty := ""
	if ci.Dirty {
		dirty = " +dirty patch"
	}
	logf("code: bundled %d files from %s (%s)%s", len(files), bundleDir, humanSize(ref.Size), dirty)
	return ci, nil
}

func humanSize(n int64) string {
	switch {
	case n > 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// runPreSubmitHook runs the Python escape hatch (recipe hooks.pre_submit).
func runPreSubmitHook(r *recipes.Recipe, recipeDir string) (*recipes.Recipe, error) {
	hook := r.Hooks.PreSubmit
	if hook == "" {
		return r, nil
	}
	if !filepath.IsAbs(hook) {
		hook = filepath.Join(recipeDir, hook)
	}
	in, _ := json.Marshal(r)
	py := "python3"
	if p := os.Getenv("MLDOJO_PYTHON"); p != "" {
		py = p
	}
	cmd := exec.Command(py, hook)
	cmd.Dir = recipeDir
	cmd.Stdin = bytes.NewReader(in)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, usageErr("pre_submit hook %s: %v", hook, err)
	}
	nr, err := recipes.ParseJSON(out)
	if err != nil {
		return nil, usageErr("pre_submit hook output: %v", err)
	}
	nr.Hooks.PreSubmit = ""
	return nr, nil
}
