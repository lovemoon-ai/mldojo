package gpu

import "testing"

// Trimmed from amd-smi 26.2.2 (ROCm 7.2.1) on an 8x W7900D node.
const (
	amdStatic = `{"gpu_data":[{"gpu":0,"asic":{"market_name":"AMD Radeon PRO W7900D","oam_id":"N/A"},
"vram":{"size":{"value":49136,"unit":"MB"},"max_bandwidth":{"value":"N/A","unit":"GB/s"}}},
{"gpu":1,"asic":{"market_name":"AMD Radeon PRO W7900D"}}]}`
	amdMetric = `{"gpu_data":[{"gpu":0,"usage":{"gfx_activity":{"value":37,"unit":"%"},"gfx_busy_inst":"N/A"},
"temperature":{"edge":{"value":28,"unit":"C"},"hotspot":{"value":33,"unit":"C"}},
"mem_usage":{"total_vram":{"value":49136,"unit":"MB"},"used_vram":{"value":20480,"unit":"MB"}}},
{"gpu":1,"usage":{"gfx_activity":"N/A"},"temperature":{"edge":"N/A","hotspot":{"value":40,"unit":"C"}},
"mem_usage":{"total_vram":{"value":48,"unit":"GB"},"used_vram":{"value":25,"unit":"MB"}}}]}`
	amdListOut = `[{"gpu":0,"bdf":"0000:03:00.0","uuid":"u-0"},{"gpu":1,"uuid":"u-1"}]`
	amdProc    = `[{"gpu":0,"process_list":[
{"process_info":{"name":"N/A","pid":3362,"memory_usage":{"vram_mem":{"value":0,"unit":"B"}}}},
{"process_info":{"name":"python3","pid":4242,"memory_usage":{"vram_mem":{"value":2147483648,"unit":"B"}}}}]},
{"gpu":1,"process_list":[{"process_info":"No running processes detected"}]}]`
)

func TestParseAMD(t *testing.T) {
	s := parseAMD([]byte(amdStatic), []byte(amdMetric), []byte(amdListOut), []byte(amdProc))
	if len(s) != 2 {
		t.Fatalf("got %d cards: %+v", len(s), s)
	}
	g := s[0]
	if g.Model != "AMD Radeon PRO W7900D" || g.UUID != "u-0" || g.Util != 37 || g.Temp != 28 ||
		g.MemUsedMB != 20480 || g.MemTotMB != 49136 {
		t.Fatalf("card 0: %+v", g)
	}
	if len(g.Procs) != 1 || g.Procs[0].PID != 4242 || g.Procs[0].Name != "python3" || g.Procs[0].MemUsedMB != 2048 {
		t.Fatalf("card 0 procs (0-VRAM KFD handles must be dropped): %+v", g.Procs)
	}
	g = s[1]
	if g.Util != 0 || g.Temp != 40 || g.MemTotMB != 48*1024 || len(g.Procs) != 0 {
		t.Fatalf("card 1: %+v", g)
	}
	if inv := Inventory(s); inv[0].MemGB != 48 {
		t.Fatalf("inventory: %+v", inv)
	}
}

func TestParseAMDBareLists(t *testing.T) {
	// Older amd-smi prints static/metric as bare lists too.
	s := parseAMD([]byte(`[{"gpu":3,"asic":{"market_name":"MI300X"}}]`),
		[]byte(`[{"gpu":3,"mem_usage":{"total_vram":{"value":196592,"unit":"MB"}}}]`), nil, nil)
	if len(s) != 1 || s[0].Index != 3 || s[0].MemTotMB != 196592 {
		t.Fatalf("%+v", s)
	}
}
