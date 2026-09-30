// Package gpu collects GPU inventory and utilization via nvidia-smi, or
// amd-smi on ROCm nodes.
package gpu

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Bin is the nvidia-smi binary (overridable for tests).
var Bin = "nvidia-smi"

// psBin is the ps binary (overridable for tests).
var psBin = "ps"

// Stats queries every GPU, and every process holding one. Neither
// nvidia-smi nor amd-smi -> no GPUs.
func Stats(ctx context.Context) []v1.GPUStat {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, Bin,
		"--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,uuid",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return amdStats(ctx)
	}
	stats := parse(string(out))
	attachProcs(ctx, stats)
	return stats
}

// attachProcs fills in who is holding each card. Utilisation and used memory
// say a card is busy; only this says by whom, and whether it is one of ours.
func attachProcs(ctx context.Context, stats []v1.GPUStat) {
	if len(stats) == 0 {
		return
	}
	out, err := exec.CommandContext(ctx, Bin,
		"--query-compute-apps=gpu_uuid,pid,process_name,used_gpu_memory",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return
	}
	pids := parseProcs(string(out), stats)
	owners := psInfo(ctx, pids)
	for i := range stats {
		for j := range stats[i].Procs {
			if o, ok := owners[stats[i].Procs[j].PID]; ok {
				stats[i].Procs[j].User, stats[i].Procs[j].Cmd = o.user, o.cmd
			}
		}
	}
}

// parseProcs attaches compute-apps rows to their cards, by UUID, and returns
// the pids so their owners can be looked up in one ps call.
func parseProcs(out string, stats []v1.GPUStat) []int {
	byUUID := map[string]int{}
	for i, s := range stats {
		if s.UUID != "" {
			byUUID[s.UUID] = i
		}
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 4 {
			continue
		}
		i, ok := byUUID[strings.TrimSpace(f[0])]
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(f[1]))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(f[2])
		// nvidia-smi prints "[Not Found]" for a process it cannot resolve,
		// which is exactly the leaked-process case worth surfacing.
		if strings.HasPrefix(name, "[") {
			name = ""
		}
		stats[i].Procs = append(stats[i].Procs, v1.GPUProc{PID: pid, Name: name, MemUsedMB: num(f[3])})
		pids = append(pids, pid)
	}
	return pids
}

type psProc struct {
	user, cmd string
	sid       int
}

// psInfo looks up the owner of each pid. A pid in another namespace -- a
// container on the same host -- simply will not be there, and that absence
// is itself the answer: nobody here can account for it.
func psInfo(ctx context.Context, pids []int) map[int]psProc {
	out := map[int]psProc{}
	if len(pids) == 0 {
		return out
	}
	args := []string{"-o", "pid=,sid=,user=,args=", "-p", joinPIDs(pids)}
	b, err := exec.CommandContext(ctx, psBin, args...).Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		sid, _ := strconv.Atoi(f[1])
		cmd := strings.Join(f[3:], " ")
		if len(cmd) > 160 {
			cmd = cmd[:160] + "…"
		}
		out[pid] = psProc{user: f[2], cmd: cmd, sid: sid}
	}
	return out
}

// Sessions returns each pid's session id, which is how a GPU process is
// traced back to the run that started it: runs are launched with setsid, so
// every descendant shares the launcher's session.
func Sessions(ctx context.Context, pids []int) map[int]int {
	out := map[int]int{}
	for pid, p := range psInfo(ctx, pids) {
		out[pid] = p.sid
	}
	return out
}

func joinPIDs(pids []int) string {
	var sb strings.Builder
	for i, p := range pids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.Itoa(p))
	}
	return sb.String()
}

func num(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func parse(out string) []v1.GPUStat {
	var stats []v1.GPUStat
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 6 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		s := v1.GPUStat{Index: idx, Model: strings.TrimSpace(f[1]), Util: num(f[2]),
			MemUsedMB: num(f[3]), MemTotMB: num(f[4]), Temp: num(f[5])}
		if len(f) > 6 {
			s.UUID = strings.TrimSpace(f[6])
		}
		stats = append(stats, s)
	}
	return stats
}

// Inventory converts stats into capacity GPU entries.
func Inventory(stats []v1.GPUStat) []v1.GPUInfo {
	out := []v1.GPUInfo{}
	for _, s := range stats {
		out = append(out, v1.GPUInfo{Index: s.Index, Model: s.Model, MemGB: int((s.MemTotMB + 512) / 1024)})
	}
	return out
}
