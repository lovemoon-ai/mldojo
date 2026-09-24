package node_local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What the probe printed on the real nodes, run against an empty home so it
// saw what a first deploy sees. testdata/real/datahome.
func probeOutput(t *testing.T, node string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "real", "datahome", node+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDataHomeOnRealNodes(t *testing.T) {
	for _, tc := range []struct {
		node, dir, why string
	}{
		// Root disk at 97% with two 9 TB data disks beside it: the one with
		// the most room, in the directory this user already owns there --
		// not the other data disk, which is writable but not theirs.
		{"gpu-b", "/mnt/data2/alice/mldojo", "home is the small disk"},
		{"gpu-a", "/mnt/data1/alice/mldojo", "home is the small disk"},
		// /home is a 28 TB local disk: home is already the right place.
		{"bastion-a", "", "home is on the largest disk"},
		{"bastion-b", "", "home is on the largest disk"},
		{"dev", "", "there is only one disk"},
		// A container: home is on the overlay, s3fs claims 16 EB free and a
		// 2 TB disk is bind-mounted as a single file at /etc/hosts. The
		// cluster filesystem is the only real candidate.
		{"h20", "/vepfs-data/fileset1/root/mldojo", "fuse and file mounts are not disks"},
	} {
		d := chooseDataHome(probeOutput(t, tc.node))
		if d.Dir != tc.dir {
			t.Errorf("%s: dir = %q (%s), want %q: %s", tc.node, d.Dir, d.Reason, tc.dir, tc.why)
		}
		if d.Reason == "" {
			t.Errorf("%s: no reason given", tc.node)
		}
	}
}

// A mount point that is a file is a bind mount of one path, not a disk --
// even when it is the biggest thing in the table.
func TestDataHomeSkipsFileMounts(t *testing.T) {
	var kept []string
	for _, l := range strings.Split(probeOutput(t, "h20"), "\n") {
		if !strings.Contains(l, "/vepfs-data/fileset1") {
			kept = append(kept, l)
		}
	}
	d := chooseDataHome(strings.Join(kept, "\n"))
	if strings.HasPrefix(d.Dir, "/etc/hosts") || strings.HasPrefix(d.Dir, "/usr/bin/nvidia-smi") {
		t.Fatalf("chose a file bind mount: %q", d.Dir)
	}
	if d.Dir != "/ebs/docker/root/mldojo" {
		t.Errorf("dir = %q, want the next real disk, /ebs/docker/root/mldojo", d.Dir)
	}
}

// Deploy also runs on upgrade. A node that already has ~/.mldojo has jobs
// writing into it; relocating that is not something a deploy should do.
func TestDataHomeLeavesAnExistingDirectory(t *testing.T) {
	out := strings.Replace(probeOutput(t, "gpu-b"), "HOMEMNT=", "EXISTS=/home/alice/.mldojo\nHOMEMNT=", 1)
	if d := chooseDataHome(out); d.Dir != "" || !strings.Contains(d.Reason, "already exists") {
		t.Errorf("existing ~/.mldojo: %+v", d)
	}
}

// <mount>/<user> that belongs to somebody else is not ours to write into, and
// creating a sibling of it would be a surprise; the next disk is used.
func TestDataHomeSkipsADirectoryThatIsNotOurs(t *testing.T) {
	out := strings.Replace(probeOutput(t, "gpu-b"),
		"UDIR\t/mnt/data2\t/mnt/data2/alice\ty\ty", "UDIR\t/mnt/data2\t/mnt/data2/alice\tn\tn", 1)
	if d := chooseDataHome(out); d.Dir != "/mnt/data3/alice/mldojo" {
		t.Errorf("dir = %q (%s), want /mnt/data3/alice/mldojo", d.Dir, d.Reason)
	}
}

// A node whose df could not be read keeps the old behaviour.
func TestDataHomeWithoutAMountTable(t *testing.T) {
	if d := chooseDataHome("USER=me\nHOME=/home/me\nDONE\n"); d.Dir != "" {
		t.Errorf("no mount table: %+v", d)
	}
}
