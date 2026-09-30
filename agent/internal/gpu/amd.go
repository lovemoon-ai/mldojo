package gpu

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// AMDBin is amd-smi, used on ROCm nodes that have no nvidia-smi.
var AMDBin = "amd-smi"

// amdStats is Stats for AMD cards. HIP honours CUDA_VISIBLE_DEVICES, so the
// rest of the agent does not need to know which vendor it is running on.
func amdStats(ctx context.Context) []v1.GPUStat {
	run := func(args ...string) []byte {
		out, _ := exec.CommandContext(ctx, AMDBin, append(args, "--json")...).Output()
		return out
	}
	static := run("static", "-a")
	if len(static) == 0 {
		return nil
	}
	stats := parseAMD(static, run("metric", "-u", "-m", "-t"), run("list"), run("process"))
	var pids []int
	for _, s := range stats {
		for _, p := range s.Procs {
			pids = append(pids, p.PID)
		}
	}
	owners := psInfo(ctx, pids)
	for i := range stats {
		for j := range stats[i].Procs {
			if o, ok := owners[stats[i].Procs[j].PID]; ok {
				stats[i].Procs[j].User, stats[i].Procs[j].Cmd = o.user, o.cmd
			}
		}
	}
	return stats
}

// amdQty is amd-smi's {"value": v, "unit": u}; "N/A" and friends decode to 0.
type amdQty struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

func (q *amdQty) UnmarshalJSON(b []byte) error {
	type plain amdQty
	var p plain
	if json.Unmarshal(b, &p) == nil {
		*q = amdQty(p)
	}
	return nil
}

func (q amdQty) mb() float64 {
	switch strings.ToUpper(q.Unit) {
	case "B":
		return q.Value / (1 << 20)
	case "KB":
		return q.Value / (1 << 10)
	case "GB":
		return q.Value * (1 << 10)
	}
	return q.Value
}

type amdGPU struct {
	GPU  int `json:"gpu"`
	ASIC struct {
		MarketName string `json:"market_name"`
	} `json:"asic"`
	UUID  string `json:"uuid"`
	Usage struct {
		GFX amdQty `json:"gfx_activity"`
	} `json:"usage"`
	Temp struct {
		Edge    amdQty `json:"edge"`
		Hotspot amdQty `json:"hotspot"`
	} `json:"temperature"`
	Mem struct {
		Total amdQty `json:"total_vram"`
		Used  amdQty `json:"used_vram"`
	} `json:"mem_usage"`
	Procs []struct {
		Info json.RawMessage `json:"process_info"`
	} `json:"process_list"`
}

// amdList decodes one amd-smi --json document. Depending on the version and
// subcommand it is either a bare list or {"gpu_data": [...]}.
func amdList(b []byte) []amdGPU {
	var list []amdGPU
	if json.Unmarshal(b, &list) == nil {
		return list
	}
	var wrapped struct {
		GPUData []amdGPU `json:"gpu_data"`
	}
	_ = json.Unmarshal(b, &wrapped)
	return wrapped.GPUData
}

func parseAMD(static, metric, list, proc []byte) []v1.GPUStat {
	var stats []v1.GPUStat
	at := map[int]int{}
	for _, g := range amdList(static) {
		at[g.GPU] = len(stats)
		stats = append(stats, v1.GPUStat{Index: g.GPU, Model: g.ASIC.MarketName})
	}
	for _, g := range amdList(metric) {
		i, ok := at[g.GPU]
		if !ok {
			continue
		}
		s := &stats[i]
		s.Util, s.MemUsedMB, s.MemTotMB = g.Usage.GFX.Value, g.Mem.Used.mb(), g.Mem.Total.mb()
		s.Temp = g.Temp.Edge.Value
		if s.Temp == 0 {
			s.Temp = g.Temp.Hotspot.Value
		}
	}
	for _, g := range amdList(list) {
		if i, ok := at[g.GPU]; ok {
			stats[i].UUID = g.UUID
		}
	}
	for _, g := range amdList(proc) {
		i, ok := at[g.GPU]
		if !ok {
			continue
		}
		for _, p := range g.Procs {
			var info struct {
				PID  int    `json:"pid"`
				Name string `json:"name"`
				Mem  struct {
					VRAM amdQty `json:"vram_mem"`
				} `json:"memory_usage"`
			}
			// "No running processes detected" is a string, not an object.
			if json.Unmarshal(p.Info, &info) != nil || info.PID == 0 {
				continue
			}
			// Every process with the KFD open is listed on every card, VRAM or
			// not; only the ones actually holding memory are worth showing.
			mb := info.Mem.VRAM.mb()
			if mb <= 0 {
				continue
			}
			if info.Name == "N/A" {
				info.Name = ""
			}
			stats[i].Procs = append(stats[i].Procs, v1.GPUProc{PID: info.PID, Name: info.Name, MemUsedMB: mb})
		}
	}
	return stats
}
