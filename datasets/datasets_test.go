package datasets

import (
	"testing"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

func TestParseLocation(t *testing.T) {
	l, err := ParseLocation("node:gpu-a:/data/pusht,authoritative", "")
	if err != nil || l.Kind != "node_path" || l.Node != "gpu-a" || l.Path != "/data/pusht" || !l.Authoritative {
		t.Fatalf("%+v %v", l, err)
	}
	l, err = ParseLocation("bucket:shared-bucket/team_lab/users/bob/datasets/pusht", "secret://buckets/team_lab")
	if err != nil || l.Bucket != "team_lab" || l.Path != "users/bob/datasets/pusht" || l.Credentials == "" {
		t.Fatalf("%+v %v", l, err)
	}
	for _, bad := range []string{"node:gpu-a", "bucket:x/y", "s3:foo"} {
		if _, err := ParseLocation(bad, ""); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if FormatLocation(v1.DatasetLocation{Kind: "node_path", Node: "a", Path: "/p", Authoritative: true}) != "node:a:/p (authoritative)" {
		t.Fatal("format")
	}
}

func TestPickSource(t *testing.T) {
	d := &v1.Dataset{Locations: []v1.DatasetLocation{
		{Kind: "bucket", Bucket: "b", Path: "p"},
		{Kind: "node_path", Node: "h20", Path: "/root/ds"},
		{Kind: "node_path", Node: "gpu-a", Path: "/data/ds", Authoritative: true},
		{Kind: "node_path", Node: "bastion-a", Path: "/cache/ds"},
	}}
	all := func(string) bool { return true }
	if s := PickSource(d, "bastion-a", all); s == nil || s.Node != "gpu-a" {
		t.Fatalf("want authoritative gpu-a, got %+v", s)
	}
	noApex := func(n string) bool { return n != "gpu-a" }
	if s := PickSource(d, "bastion-a", noApex); s == nil || s.Node != "h20" {
		t.Fatalf("want h20 fallback, got %+v", s)
	}
	if s := PickSource(d, "x", func(string) bool { return false }); s != nil {
		t.Fatal("no online source expected")
	}
	if len(OnNode(d, "gpu-a")) != 1 || BucketLocation(d).Bucket != "b" {
		t.Fatal("helpers")
	}
	if CachePath("", "pusht", "v1") != "~/.mldojo/datasets/pusht/v1" {
		t.Fatal("cache path")
	}
}
