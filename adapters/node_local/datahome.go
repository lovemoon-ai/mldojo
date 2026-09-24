package node_local

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Where ~/.mldojo lives on a node.
//
// Everything that grows is under it: run workdirs with their checkpoints,
// the dataset cache, the venv/conda cache. Left in home it fills the home
// disk, and on real nodes home is often the small one: gpu-b's root disk
// was at 97% with 9 TB of data disk beside it. So a node that has never had
// an agent gets ~/.mldojo as a symlink into a directory of this user's on the
// data disk with the most room. A node that already has ~/.mldojo keeps it:
// moving a directory running jobs are writing to is not a deploy step.

// DataHome is the decision.
type DataHome struct {
	// Dir is where ~/.mldojo should point. Empty means leave it in home.
	Dir    string  `json:"dir,omitempty"`
	Mount  string  `json:"mount,omitempty"`
	FreeGB float64 `json:"free_gb,omitempty"`
	Reason string  `json:"reason"`
}

// dataHomeProbe is read-only. It reports the mount table and, for each
// mount, whether this user has somewhere of their own on it; the choice is
// made in Go so it can be tested against what real nodes printed.
const dataHomeProbe = `u=$(id -un)
T=""; command -v timeout >/dev/null 2>&1 && T="timeout 10"
echo "USER=$u"
echo "HOME=$HOME"
if [ -L "$HOME/.mldojo" ] || [ -e "$HOME/.mldojo" ]; then echo "EXISTS=$(readlink -f "$HOME/.mldojo" 2>/dev/null || echo "$HOME/.mldojo")"; fi
echo "HOMEMNT=$($T df -P "$HOME" 2>/dev/null | awk 'NR==2{print $NF}')"
DF=$($T df -PTk 2>/dev/null | awk 'NR>1 && NF>=7 {print "FS\t" $2 "\t" $5 "\t" $7}')
printf '%s\n' "$DF"
printf '%s\n' "$DF" | while IFS="$(printf '\t')" read -r _ _ _ m; do
  [ -n "$m" ] && [ -d "$m" ] || continue
  w=n; [ -w "$m" ] && w=y
  printf 'MNT\t%s\t%s\n' "$m" "$w"
  for c in "$m/$u" "$m/users/$u" "$m/home/$u"; do
    [ -d "$c" ] || continue
    o=n; [ -O "$c" ] && o=y; cw=n; [ -w "$c" ] && cw=y
    printf 'UDIR\t%s\t%s\t%s\t%s\n' "$m" "$c" "$o" "$cw"
  done
done
echo DONE`

// Filesystems that are never a place to keep runs. FUSE is excluded as a
// whole: on real nodes it is object storage (s3fs reports 16 EB free, which
// would win any comparison) or a cluster-wide bucket, neither of which wants
// venvs, sockets and checkpoints being rewritten in place.
var skipFS = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "ramfs": true, "overlay": true, "squashfs": true,
	"proc": true, "sysfs": true, "cgroup": true, "cgroup2": true, "devpts": true, "mqueue": true,
	"nsfs": true, "efivarfs": true, "autofs": true, "iso9660": true, "vfat": true, "debugfs": true,
	"tracefs": true, "securityfs": true, "pstore": true, "bpf": true, "configfs": true, "hugetlbfs": true,
}

var skipMount = []string{"/boot", "/proc", "/sys", "/dev", "/run", "/snap", "/var/lib/docker", "/var/lib/containers"}

// chooseDataHome reads the probe output and decides.
func chooseDataHome(out string) DataHome {
	var user, homeMnt, exists string
	type fs struct {
		mount, typ string
		availKB    float64
		dir, rw    bool
	}
	var fss []*fs
	byMount := map[string]*fs{}
	udirs := map[string][][3]string{} // mount -> [path, owned, writable]
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "USER="):
			user = strings.TrimPrefix(line, "USER=")
		case strings.HasPrefix(line, "EXISTS="):
			exists = strings.TrimPrefix(line, "EXISTS=")
		case strings.HasPrefix(line, "HOMEMNT="):
			homeMnt = strings.TrimPrefix(line, "HOMEMNT=")
		}
		f := strings.Split(line, "\t")
		switch {
		case f[0] == "FS" && len(f) == 4:
			kb, _ := strconv.ParseFloat(f[2], 64)
			if byMount[f[3]] == nil {
				x := &fs{mount: f[3], typ: f[1], availKB: kb}
				fss = append(fss, x)
				byMount[f[3]] = x
			}
		case f[0] == "MNT" && len(f) == 3:
			if x := byMount[f[1]]; x != nil {
				x.dir, x.rw = true, f[2] == "y"
			}
		case f[0] == "UDIR" && len(f) == 5:
			udirs[f[1]] = append(udirs[f[1]], [3]string{path.Clean(f[2]), f[3], f[4]})
		}
	}
	if exists != "" {
		return DataHome{Reason: "~/.mldojo already exists (" + exists + "); left where it is"}
	}
	if user == "" || len(fss) == 0 {
		return DataHome{Reason: "could not read the mount table; staying in home"}
	}
	var cands []*fs
	for _, x := range fss {
		if !x.dir || skipFS[x.typ] || strings.HasPrefix(x.typ, "fuse") || skipped(x.mount) {
			continue
		}
		cands = append(cands, x)
	}
	// The disk with the most room first. Free space rather than size is
	// what decides whether the next checkpoint fits.
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].availKB > cands[j].availKB })
	for _, x := range cands {
		gb := x.availKB / (1 << 20)
		if x.mount == homeMnt {
			return DataHome{Mount: x.mount, FreeGB: gb,
				Reason: fmt.Sprintf("home is already on the disk with the most room (%s, %.0f GB free)", x.mount, gb)}
		}
		dir := userDirOn(x.mount, user, udirs[x.mount], x.rw)
		if dir == "" {
			continue // nowhere of this user's here; try the next disk
		}
		return DataHome{Dir: path.Join(dir, "mldojo"), Mount: x.mount, FreeGB: gb,
			Reason: fmt.Sprintf("%s has the most room (%.0f GB free)", x.mount, gb)}
	}
	return DataHome{Reason: "no data disk with a directory this user can write; staying in home"}
}

// userDirOn picks this user's directory on a mount: one that exists and is
// theirs, or <mount>/<user> to be created when the mount itself is writable.
func userDirOn(mount, user string, found [][3]string, mountWritable bool) string {
	own := path.Join(mount, user)
	for _, want := range []string{own, path.Join(mount, "users", user), path.Join(mount, "home", user)} {
		for _, u := range found {
			if u[0] == want && u[1] == "y" && u[2] == "y" {
				return want
			}
		}
	}
	for _, u := range found {
		if u[0] == own {
			return "" // <mount>/<user> exists but is not ours to write
		}
	}
	if mountWritable {
		return own
	}
	return ""
}

func skipped(mount string) bool {
	for _, s := range skipMount {
		if mount == s || strings.HasPrefix(mount, s+"/") {
			return true
		}
	}
	return false
}

// PlanDataHome runs the probe. It never fails a deploy: a node it cannot
// read simply keeps ~/.mldojo in home, as before.
func PlanDataHome(ctx context.Context, ex Executor) DataHome {
	out, err := ex.Run(ctx, dataHomeProbe, nil)
	if err != nil || !strings.Contains(out, "DONE") {
		return DataHome{Reason: "could not probe the disks; staying in home"}
	}
	return chooseDataHome(out)
}

// placeDataHome creates the directory and links ~/.mldojo to it, then checks
// the link is really there: some bastions drop commands they dislike and
// still exit 0, so the effect is verified rather than the exit code trusted.
func placeDataHome(ctx context.Context, ex Executor, dir string) error {
	q := shq(dir)
	out, err := ex.Run(ctx, `mkdir -p `+q+` && { [ -e "$HOME/.mldojo" ] || [ -L "$HOME/.mldojo" ] || ln -s `+q+` "$HOME/.mldojo"; }
[ "$(readlink "$HOME/.mldojo")" = `+q+` ] && [ -d "$HOME/.mldojo/" ] && echo LINKED`, nil)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "LINKED") {
		return fmt.Errorf("~/.mldojo is not a link to %s after linking (output: %q)", dir, strings.TrimSpace(out))
	}
	return nil
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
