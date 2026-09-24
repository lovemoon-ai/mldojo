// Package datasets holds the dataset-manager rules shared by the API and the
// CLI: references, location syntax, node cache layout and the
// choice of a sync source.
package datasets

import (
	"fmt"
	"path"
	"strings"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// CompleteMarker is written into a node cache directory once a sync finished.
const CompleteMarker = ".mldojo_complete"

// SplitRef parses "name@version" (version may be empty = latest).
func SplitRef(ref string) (name, version string) {
	name, version, _ = strings.Cut(ref, "@")
	return name, version
}

// ParseLocation parses the CLI syntax:
//
//	node:<node>:<path>
//	bucket:<provider>/<bucket>/<path>
//
// A trailing ",authoritative" marks the authoritative copy.
func ParseLocation(s, bucketCreds string) (v1.DatasetLocation, error) {
	auth := false
	if strings.HasSuffix(s, ",authoritative") {
		s, auth = strings.TrimSuffix(s, ",authoritative"), true
	}
	kind, rest, _ := strings.Cut(s, ":")
	switch kind {
	case "node":
		node, p, ok := strings.Cut(rest, ":")
		if !ok || node == "" || p == "" {
			return v1.DatasetLocation{}, fmt.Errorf("location %q: expected node:<node>:<path>", s)
		}
		return v1.DatasetLocation{Kind: "node_path", Node: node, Path: p, Authoritative: auth}, nil
	case "bucket":
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
			return v1.DatasetLocation{}, fmt.Errorf("location %q: expected bucket:<provider>/<bucket>/<path>", s)
		}
		return v1.DatasetLocation{Kind: "bucket", Provider: parts[0], Bucket: parts[1], Path: parts[2],
			Credentials: bucketCreds, Authoritative: auth}, nil
	}
	return v1.DatasetLocation{}, fmt.Errorf("location %q: kind must be node or bucket", s)
}

// FormatLocation renders a location in the CLI syntax.
func FormatLocation(l v1.DatasetLocation) string {
	s := fmt.Sprintf("node:%s:%s", l.Node, l.Path)
	if l.Kind == "bucket" {
		s = fmt.Sprintf("bucket:%s/%s/%s", l.Provider, l.Bucket, l.Path)
	}
	if l.Authoritative {
		s += " (authoritative)"
	}
	return s
}

// CachePath is where a node caches name@version under its cache root.
func CachePath(root, name, version string) string {
	if root == "" {
		root = "~/.mldojo/datasets"
	}
	return path.Join(root, name, version)
}

// OnNode returns the registered node_path locations on a node.
func OnNode(d *v1.Dataset, node string) []v1.DatasetLocation {
	var out []v1.DatasetLocation
	for _, l := range d.Locations {
		if l.Kind == "node_path" && l.Node == node {
			out = append(out, l)
		}
	}
	return out
}

// PickSource chooses where to copy from when node lacks the dataset: a
// node_path on another node whose agent is online, preferring the
// authoritative one. Bucket locations are never synced to nodes in v1.
func PickSource(d *v1.Dataset, node string, online func(node string) bool) *v1.DatasetLocation {
	var src *v1.DatasetLocation
	for i, l := range d.Locations {
		if l.Kind != "node_path" || l.Node == node || !online(l.Node) {
			continue
		}
		if src == nil || (l.Authoritative && !src.Authoritative) {
			src = &d.Locations[i]
		}
	}
	return src
}

// BucketLocation returns the first bucket location (what queue plugins mount), or nil.
func BucketLocation(d *v1.Dataset) *v1.DatasetLocation {
	for i, l := range d.Locations {
		if l.Kind == "bucket" {
			return &d.Locations[i]
		}
	}
	return nil
}
