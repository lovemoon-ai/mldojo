package commands

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lovemoon-ai/mldojo/cli/internal/client"
	"github.com/lovemoon-ai/mldojo/cli/internal/output"
	"github.com/lovemoon-ai/mldojo/datasets"
	"github.com/lovemoon-ai/mldojo/internal/config"
	"github.com/lovemoon-ai/mldojo/internal/keychain"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ---- nodes -------------------------------------------------------------------

func gpuSummary(n v1.Node) string {
	if n.Capacity == nil || len(n.Capacity.GPUs) == 0 {
		return "-"
	}
	counts := map[string]int{}
	for _, g := range n.Capacity.GPUs {
		counts[strings.TrimPrefix(strings.TrimPrefix(g.Model, "NVIDIA "), "GeForce ")]++
	}
	var parts []string
	for m, c := range counts {
		parts = append(parts, fmt.Sprintf("%dx %s", c, m))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// splitSSH splits user@host on the LAST '@' so bastion users like
// "bob@bob@198.51.100.20@bastion.example.com" work.
func splitSSH(s string) (user, host string) {
	if i := strings.LastIndex(s, "@"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

func (a *app) nodeLimitCmd() *cobra.Command {
	var max int
	c := &cobra.Command{
		Use: "limit <node-id>", Short: "Cap how many runs a node takes at once (0 = unlimited)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var n v1.Node
			if err := a.client().Do(cmd.Context(), "PATCH", "/nodes/"+url.PathEscape(args[0])+"/limit",
				map[string]int{"max_runs": max}, &n); err != nil {
				return err
			}
			return a.out().Emit(n, func(w ioWriter) {
				if n.MaxRuns == 0 {
					fmt.Fprintf(w, "%s: unlimited concurrent runs\n", n.ID)
					return
				}
				fmt.Fprintf(w, "%s: at most %d concurrent runs\n", n.ID, n.MaxRuns)
			})
		},
	}
	c.Flags().IntVar(&max, "max-runs", 0, "concurrent run limit, 0 for unlimited")
	return c
}

func (a *app) nodeCmd() *cobra.Command {
	c := &cobra.Command{Use: "node", Aliases: []string{"nodes"}, Short: "Manage compute nodes (agents)"}
	var req struct {
		id, display, ssh, identity, password, labels, proxy, noProxy, workdir, datasets, agentURL string
		port                                                                                      int
		via                                                                                       []string
		local, reverse, dryRun                                                                    bool
		file                                                                                      string
	}
	add := &cobra.Command{
		Use:   "add --id <id> (--ssh user@host | --local)",
		Short: "Deploy an agent to a node and register it (fails without storing anything if unreachable)",
		Example: `  mldojo node add --id local --local
  mldojo node add --id gpu-a --ssh alice@192.0.2.10 --labels 5090
  mldojo node add --id bastion-a --ssh "bob@bob@198.51.100.20@bastion.example.com" --port 2222 \
      --identity secret://ssh_keys/id_rsa_deploy --via gpu-a --via local --labels 5090,8gpu,bastion`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if req.id == "" && req.local {
				req.id = "local"
			}
			body := map[string]any{}
			if req.file != "" {
				// A node definition in YAML; flags override it.
				b, err := os.ReadFile(req.file)
				if err != nil {
					return usageErr("%v", err)
				}
				var m map[string]any
				if err := yaml.Unmarshal(b, &m); err != nil {
					return usageErr("%s: %v", req.file, err)
				}
				body = normalizeYAML(m).(map[string]any)
				delete(body, "capacity") // reported by the agent
				delete(body, "agent")
				if ls, ok := body["labels"].([]any); ok { // YAML turns 5090 into a number
					for i := range ls {
						ls[i] = fmt.Sprint(ls[i])
					}
				}
			}
			changed := cmd.Flags().Changed
			for flag, key := range map[string]string{"id": "id", "display-name": "display_name", "workdir-root": "workdir_root", "datasets-root": "datasets_cache_root"} {
				if changed(flag) || (flag == "id" && req.id != "" && body["id"] == nil) {
					body[key] = map[string]string{"id": req.id, "display_name": req.display, "workdir_root": req.workdir, "datasets_cache_root": req.datasets}[key]
				}
			}
			if body["id"] == nil || body["id"] == "" {
				return usageErr("--id is required")
			}
			req.id = fmt.Sprint(body["id"])
			if changed("labels") {
				body["labels"] = strings.Split(req.labels, ",")
			}
			if req.ssh != "" || req.local || body["connection"] == nil {
				conn := v1.NodeConnection{Type: "ssh", Port: req.port, Identity: req.identity, Password: req.password,
					AgentServerURL: req.agentURL, ReverseTunnel: req.reverse}
				if req.local {
					conn.Type = "local"
				} else {
					if req.ssh == "" {
						return usageErr("--ssh user@host, --local or -f node.yaml is required")
					}
					conn.User, conn.Host = splitSSH(req.ssh)
					for _, v := range req.via {
						for _, x := range strings.Split(v, ",") {
							if x = strings.TrimSpace(x); x != "" {
								conn.Via = append(conn.Via, v1.Via{Node: x})
							}
						}
					}
				}
				body["connection"] = conn
			}
			if req.proxy != "" {
				p := v1.Proxy{HTTP: req.proxy, HTTPS: req.proxy}
				if req.noProxy != "" {
					p.NoProxy = strings.Split(req.noProxy, ",")
				}
				body["proxy"] = p
			}
			if req.dryRun {
				body["dry_run"] = true
				var res map[string]any
				if err := a.client().Do(cmd.Context(), "POST", "/nodes", body, &res); err != nil {
					return err
				}
				return a.out().Emit(res, func(w ioWriter) {
					p, _ := res["probe"].(map[string]any)
					output.KV(w, [2]string{"reachable", "yes"}, [2]string{"route", fmt.Sprint(res["route"])},
						[2]string{"latency", fmt.Sprintf("%vms", res["latency_ms"])}, [2]string{"host", fmt.Sprintf("%v (%v/%v)", p["hostname"], p["os"], p["arch"])},
						[2]string{"gpus", output.Or(fmt.Sprint(p["gpus"]), "-")}, [2]string{"python", fmt.Sprint(p["python"])},
						[2]string{"docker/conda", fmt.Sprintf("%v/%v", p["docker"], p["conda"])})
					if rt, ok := res["reverse_tunnel"]; ok {
						output.KV(w, [2]string{"reverse tunnel", fmt.Sprint(rt)})
					}
					if dh, _ := p["data_home"].(map[string]any); dh != nil {
						where := "stays in home"
						if dir, _ := dh["dir"].(string); dir != "" {
							where = "~/.mldojo -> " + dir
						}
						output.KV(w, [2]string{"data home", fmt.Sprintf("%s (%v)", where, dh["reason"])})
					}
					fmt.Fprintln(w, "(dry run: nothing was deployed)")
				})
			}
			if !a.jsonOut {
				fmt.Fprintf(os.Stderr, "deploying agent to %s (up to ~60s)...\n", req.id)
			}
			var n v1.Node
			if err := a.client().Do(cmd.Context(), "POST", "/nodes", body, &n); err != nil {
				return err
			}
			return a.out().Emit(n, func(w ioWriter) {
				fmt.Fprintf(w, "node %s online: %s, %d CPU, %d GiB RAM (agent %s)\n", n.ID, gpuSummary(n), n.Capacity.CPU, n.Capacity.MemGB, n.AgentVersion)
				if n.Capacity.MldojoDir != "" {
					fmt.Fprintf(w, "runs, datasets and envs live in %s\n", n.Capacity.MldojoDir)
				}
			})
		},
	}
	f := add.Flags()
	f.StringVar(&req.id, "id", "", "node id (used in targets: node:<id>)")
	f.StringVar(&req.display, "display-name", "", "display name")
	f.StringVar(&req.ssh, "ssh", "", "ssh user@host (the last '@' separates the host)")
	f.IntVar(&req.port, "port", 0, "ssh port (default 22)")
	f.StringVar(&req.identity, "identity", "", "private key: secret://ssh_keys/<name> or a path on the API host")
	f.StringVar(&req.password, "password", "", "password reference: secret://passwords/<name>")
	f.StringArrayVar(&req.via, "via", nil, "jump through node(s), tried in order; 'local' = direct")
	f.StringVar(&req.labels, "labels", "", "comma separated labels")
	f.StringVar(&req.proxy, "proxy", "", "http(s) proxy for runs on this node")
	f.StringVar(&req.noProxy, "no-proxy", "", "comma separated no_proxy list")
	f.StringVar(&req.workdir, "workdir-root", "", "run workdirs on the node (default ~/.mldojo/runs)")
	f.StringVar(&req.datasets, "datasets-root", "", "dataset cache on the node (default ~/.mldojo/datasets)")
	f.StringVar(&req.agentURL, "agent-server-url", "", "URL the agent dials back to (default: server public_url)")
	f.BoolVar(&req.reverse, "reverse-tunnel", false, "tunnel the agent connection back over SSH (node cannot reach the server)")
	f.BoolVar(&req.local, "local", false, "the API host itself")
	f.BoolVar(&req.dryRun, "dry-run", false, "only test the connection (via chain, auth) and probe the node; deploy nothing")
	f.StringVarP(&req.file, "file", "f", "", "node definition YAML; flags override it")

	ls := &cobra.Command{
		Use: "ls", Short: "List nodes", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ns []v1.Node
			if err := a.client().Get(cmd.Context(), "/nodes", &ns); err != nil {
				return err
			}
			return a.out().Emit(ns, func(w ioWriter) {
				rows := [][]string{}
				for _, n := range ns {
					util := "-"
					if len(n.GPUStats) > 0 {
						var parts []string
						for _, g := range n.GPUStats {
							parts = append(parts, fmt.Sprintf("%.0f%%", g.Util))
						}
						util = strings.Join(parts, " ")
					}
					rows = append(rows, []string{n.ID, n.AgentStatus, gpuSummary(n), util, strconv.Itoa(n.ActiveRuns),
						strings.Join(n.Labels, ","), output.Ago(n.LastHeartbeat)})
				}
				output.Table(w, []string{"ID", "AGENT", "GPUS", "UTIL", "ACTIVE", "LABELS", "HEARTBEAT"}, rows)
			})
		},
	}
	show := &cobra.Command{
		Use: "show <id>", Short: "Show a node", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var n v1.Node
			if err := a.client().Get(cmd.Context(), "/nodes/"+url.PathEscape(args[0]), &n); err != nil {
				return err
			}
			return a.out().Emit(n, func(w ioWriter) {
				conn, _ := json.Marshal(n.Connection)
				output.KV(w, [2]string{"id", n.ID}, [2]string{"agent", n.AgentStatus + " " + n.AgentVersion},
					[2]string{"gpus", gpuSummary(n)}, [2]string{"labels", strings.Join(n.Labels, ",")},
					[2]string{"connection", string(conn)}, [2]string{"workdir_root", n.WorkdirRoot},
					[2]string{"datasets_cache_root", n.DatasetsCacheRoot}, [2]string{"active runs", strconv.Itoa(n.ActiveRuns)},
					[2]string{"heartbeat", output.Ago(n.LastHeartbeat)})
				if n.Capacity != nil {
					output.KV(w, [2]string{"cpu/mem", fmt.Sprintf("%d CPU, %d GiB RAM", n.Capacity.CPU, n.Capacity.MemGB)},
						[2]string{"host", n.Capacity.Hostname + " (" + n.Capacity.OS + "/" + n.Capacity.Arch + ")"})
					if n.Capacity.MldojoDir != "" {
						output.KV(w, [2]string{"mldojo dir", n.Capacity.MldojoDir})
					}
				}
				printDisks(w, &n)
				fmt.Fprintln(w)
				printGPU(w, n.GPUStats)
			})
		},
	}
	var keepAgent, force bool
	rm := &cobra.Command{
		Use: "rm <id>", Short: "Remove a node (stops its agent)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if keepAgent {
				q.Set("stop_agent", "0")
			}
			if force {
				q.Set("force", "1")
			}
			if err := a.client().Do(cmd.Context(), "DELETE", "/nodes/"+url.PathEscape(args[0])+"?"+q.Encode(), nil, nil); err != nil {
				return err
			}
			return a.out().Emit(map[string]bool{"ok": true}, func(w ioWriter) { fmt.Fprintf(w, "removed node %s\n", args[0]) })
		},
	}
	rm.Flags().BoolVar(&keepAgent, "keep-agent", false, "leave the agent process running")
	rm.Flags().BoolVar(&force, "force", false, "remove even with active runs")
	var watch bool
	gpuC := &cobra.Command{
		Use: "gpu <id>", Short: "Live GPU status of a node", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !watch {
				var g []v1.GPUStat
				if err := a.client().Get(cmd.Context(), "/nodes/"+url.PathEscape(args[0])+"/gpu", &g); err != nil {
					return err
				}
				return a.out().Emit(g, func(w ioWriter) { printGPU(w, g) })
			}
			ws, err := a.client().WS(cmd.Context(), "/nodes/"+url.PathEscape(args[0])+"/gpu/ws", nil)
			if err != nil {
				return err
			}
			defer ws.Close()
			go func() { <-cmd.Context().Done(); ws.Close() }()
			for {
				var fr v1.Frame
				if err := ws.ReadJSON(&fr); err != nil {
					return nil
				}
				var g []v1.GPUStat
				json.Unmarshal(fr.Payload, &g)
				a.out().Emit(g, func(w ioWriter) {
					fmt.Fprintf(w, "\033[H\033[2J%s  %s\n", args[0], time.Now().Format(time.TimeOnly))
					printGPU(w, g)
				})
			}
		},
	}
	gpuC.Flags().BoolVarP(&watch, "watch", "w", false, "stream updates")

	var since string
	var maxPts int
	histC := &cobra.Command{
		Use: "history <id>", Short: "GPU utilization over time (what the node was doing, not what it is doing)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"since": {since}, "max_points": {strconv.Itoa(maxPts)}}
			var h []gpuSeries
			if err := a.client().Get(cmd.Context(),
				"/nodes/"+url.PathEscape(args[0])+"/gpu/history?"+q.Encode(), &h); err != nil {
				return err
			}
			return a.out().Emit(h, func(w ioWriter) { printGPUHistory(w, h) })
		},
	}
	histC.Flags().StringVar(&since, "since", "6h", "window start: a duration like 24h, or an RFC3339 time")
	histC.Flags().IntVar(&maxPts, "max-points", 500, "points per card")

	diskC := &cobra.Command{
		Use: "disk <id>", Short: "What mldojo is holding on a node, per run",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res nodeDiskResp
			if err := a.client().Get(cmd.Context(), "/nodes/"+url.PathEscape(args[0])+"/disk", &res); err != nil {
				return err
			}
			return a.out().Emit(res, func(w ioWriter) {
				fmt.Fprintf(w, "%s\n\n", res.Usage.Root)
				if len(res.Usage.Entries) == 0 {
					fmt.Fprintln(w, "no run directories")
					return
				}
				rows := [][]string{}
				var total int64
				for _, e := range res.Usage.Entries {
					total += e.Bytes
					who, status := "-", "-"
					if r, ok := res.Runs[e.RunID]; ok {
						who, status = r.Project+"/"+r.Experiment+" "+r.Name, r.Status
					}
					rows = append(rows, []string{output.Short(e.RunID), who, status,
						output.Bytes(e.Bytes), strconv.Itoa(e.Files), output.Ago(&e.Modified)})
				}
				output.Table(w, []string{"RUN", "EXPERIMENT", "STATUS", "SIZE", "FILES", "MODIFIED"}, rows)
				fmt.Fprintf(w, "\ntotal %s in %d run directories\n", output.Bytes(total), len(res.Usage.Entries))
				if res.Usage.Truncated {
					fmt.Fprintln(w, "(walk hit its file budget; the total is a floor)")
				}
				fmt.Fprintln(w, "reclaim one with: mldojo run rm <run> --purge-node")
			})
		},
	}
	test := &cobra.Command{
		Use: "test <id>", Short: "Test SSH reachability and agent health", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res map[string]any
			if err := a.client().Do(cmd.Context(), "POST", "/nodes/"+url.PathEscape(args[0])+"/test", map[string]any{}, &res); err != nil {
				return err
			}
			if err := a.out().Emit(res, func(w ioWriter) {
				output.KV(w, [2]string{"ok", fmt.Sprint(res["ok"])}, [2]string{"reachable", fmt.Sprint(res["reachable"])},
					[2]string{"agent", fmt.Sprint(res["agent_online"])}, [2]string{"route", fmt.Sprint(res["via"])},
					[2]string{"latency", fmt.Sprintf("%vms", res["latency_ms"])}, [2]string{"message", fmt.Sprint(res["message"])})
			}); err != nil {
				return err
			}
			if res["ok"] != true {
				return &client.Error{Code: "unreachable", Msg: fmt.Sprintf("node %s: %v", args[0], res["message"])}
			}
			return nil
		},
	}
	upgrade := &cobra.Command{
		Use: "upgrade <id>...", Short: "Redeploy the current agent binary (running jobs are re-adopted)", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out []v1.Node
			for _, id := range args {
				var n v1.Node
				if err := a.client().Do(cmd.Context(), "POST", "/nodes/"+url.PathEscape(id)+"/upgrade", map[string]any{}, &n); err != nil {
					return err
				}
				out = append(out, n)
			}
			return a.out().Emit(out, func(w ioWriter) {
				for _, n := range out {
					fmt.Fprintf(w, "node %s: agent %s %s\n", n.ID, n.AgentVersion, n.AgentStatus)
				}
			})
		},
	}
	c.AddCommand(add, ls, show, rm, gpuC, histC, diskC, test, upgrade, a.nodeLimitCmd())
	return c
}

// printDisks shows every filesystem the node writes to, not one number. A
// job can keep its workdir on a small container overlay and its checkpoints
// on a shared cluster filesystem; a single figure hides that, and the one
// from Capacity is a snapshot from whenever the agent last reconnected.
func printDisks(w io.Writer, n *v1.Node) {
	if len(n.Disks) == 0 {
		if n.Capacity != nil {
			output.KV(w, [2]string{"disk", fmt.Sprintf("%d GiB free (at connect time)", n.Capacity.DiskGB)})
		}
		return
	}
	rows := [][]string{}
	for _, d := range n.Disks {
		used := 0.0
		if d.TotalGB > 0 {
			used = (d.TotalGB - d.FreeGB) / d.TotalGB * 100
		}
		rows = append(rows, []string{d.Path, d.Mount,
			fmt.Sprintf("%.0f GiB", d.TotalGB), fmt.Sprintf("%.0f GiB", d.FreeGB), fmt.Sprintf("%.0f%%", used)})
	}
	fmt.Fprintln(w)
	output.Table(w, []string{"PATH", "MOUNT", "SIZE", "FREE", "USED"}, rows)
}

func printGPU(w io.Writer, g []v1.GPUStat) {
	if len(g) == 0 {
		fmt.Fprintln(w, "no GPUs reported")
		return
	}
	rows := [][]string{}
	for _, s := range g {
		rows = append(rows, []string{strconv.Itoa(s.Index), s.Model, fmt.Sprintf("%.0f%%", s.Util),
			fmt.Sprintf("%.0f/%.0f MiB", s.MemUsedMB, s.MemTotMB), fmt.Sprintf("%.0f MiB", s.FreeMB()),
			fmt.Sprintf("%.0fC", s.Temp), strings.Join(shortIDs(s.RunIDs), ",")})
	}
	output.Table(w, []string{"GPU", "MODEL", "UTIL", "MEMORY", "FREE", "TEMP", "RUNS"}, rows)
	printProcs(w, g)
}

// printProcs lists everything holding a card and says whose it is. A card
// reporting 92 GiB used with an empty RUNS column is a dead end -- you can
// see the memory is gone but not to whom, and finding out meant sshing in.
func printProcs(w io.Writer, g []v1.GPUStat) {
	rows := [][]string{}
	for _, s := range g {
		ours := len(s.RunIDs) > 0
		for _, p := range s.Procs {
			who, user := "-", p.User
			switch {
			case p.RunID != "":
				who = output.Short(p.RunID)
			case ours && p.Unresolved():
				// nvidia-smi reported a pid from a namespace this node
				// cannot see. On a card we did put a run on, that is most
				// likely the run itself; the "?" says we are inferring.
				who = output.Short(s.RunIDs[0]) + " ?"
			}
			if user == "" {
				user = "(not in this namespace)"
			}
			rows = append(rows, []string{strconv.Itoa(s.Index), strconv.Itoa(p.PID), who, user,
				output.Or(p.Name, "-"), fmt.Sprintf("%.0f MiB", p.MemUsedMB)})
		}
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w, "\nprocesses on these cards:")
	output.Table(w, []string{"GPU", "PID", "RUN", "USER", "PROCESS", "MEMORY"}, rows)
}

func shortIDs(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = output.Short(id)
	}
	return out
}

// ---- queues ------------------------------------------------------------------

func (a *app) queueCmd() *cobra.Command {
	c := &cobra.Command{Use: "queue", Aliases: []string{"queues"}, Short: "Manage submission queues (provided by queue plugins)"}
	var id, backend, creds, jobPw, projectID, defaultsFile, labels, display string
	add := &cobra.Command{
		Use:     "add --id <plugin>/<queue> --credentials secret://<plugin>/default",
		Short:   "Register a queue",
		Example: "  mldojo queue add --id myqueue/gpu-a100 \\\n      --credentials secret://myqueue/default --job-password secret://myqueue/job-password \\\n      --project-id my-project --defaults-file q.yaml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := v1.Queue{ID: id, Backend: backend, DisplayName: display, Defaults: map[string]any{}, CapacityHint: map[string]any{}}
			if defaultsFile != "" {
				b, err := os.ReadFile(defaultsFile)
				if err != nil {
					return usageErr("%v", err)
				}
				var m map[string]any
				if err := yaml.Unmarshal(b, &m); err != nil {
					return usageErr("defaults file: %v", err)
				}
				if _, full := m["defaults"]; full || m["client"] != nil {
					// A full queue definition (v1.Queue shape).
					jb, _ := json.Marshal(normalizeYAML(m))
					if err := json.Unmarshal(jb, &q); err != nil {
						return usageErr("queue file: %v", err)
					}
				} else {
					q.Defaults = normalizeYAML(m).(map[string]any)
				}
			}
			if id != "" {
				q.ID = id
			}
			if backend != "" {
				q.Backend = backend
			}
			if creds != "" {
				q.Client.Credentials = creds
			}
			if jobPw != "" {
				q.Client.JobPassword = jobPw
			}
			if projectID != "" {
				q.Client.ProjectID = projectID
			}
			if labels != "" {
				q.Labels = strings.Split(labels, ",")
			}
			if q.ID == "" {
				return usageErr("--id is required (<plugin>/<queue name>)")
			}
			var out v1.Queue
			if err := a.client().Do(cmd.Context(), "POST", "/queues", q, &out); err != nil {
				return err
			}
			return a.out().Emit(out, func(w ioWriter) { fmt.Fprintf(w, "registered queue %s (target queue:%s)\n", out.ID, out.ID) })
		},
	}
	f := add.Flags()
	f.StringVar(&id, "id", "", "queue id: <plugin>/<queue name>")
	f.StringVar(&backend, "backend", "", "queue plugin name (default: the id prefix)")
	f.StringVar(&creds, "credentials", "", "secret:// reference for the SDK credentials/token")
	f.StringVar(&jobPw, "job-password", "", "secret:// reference for the job password")
	f.StringVar(&projectID, "project-id", "", "project id on the scheduler, passed to the plugin")
	f.StringVar(&defaultsFile, "defaults-file", "", "YAML with defaults (docker_image, cpu_per_worker, buckets...) or a full queue definition")
	f.StringVar(&labels, "labels", "", "comma separated labels")
	f.StringVar(&display, "display-name", "", "display name")
	ls := &cobra.Command{
		Use: "ls", Short: "List queues", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var qs []v1.Queue
			if err := a.client().Get(cmd.Context(), "/queues", &qs); err != nil {
				return err
			}
			return a.out().Emit(qs, func(w ioWriter) {
				rows := [][]string{}
				for _, q := range qs {
					rows = append(rows, []string{q.ID, q.Backend, strconv.Itoa(q.ActiveRuns), fmt.Sprint(q.Defaults["docker_image"]), strings.Join(q.Labels, ",")})
				}
				output.Table(w, []string{"ID", "BACKEND", "ACTIVE", "IMAGE", "LABELS"}, rows)
			})
		},
	}
	show := &cobra.Command{
		Use: "show <id>", Short: "Show a queue", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var q v1.Queue
			if err := a.client().Get(cmd.Context(), "/queues/"+client.PathEscape(args[0]), &q); err != nil {
				return err
			}
			return a.out().Emit(q, func(w ioWriter) {
				b, _ := yaml.Marshal(q)
				w.Write(b)
			})
		},
	}
	rm := &cobra.Command{
		Use: "rm <id>", Short: "Remove a queue", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.client().Do(cmd.Context(), "DELETE", "/queues/"+client.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			return a.out().Emit(map[string]bool{"ok": true}, func(w ioWriter) { fmt.Fprintf(w, "removed queue %s\n", args[0]) })
		},
	}
	c.AddCommand(add, ls, show, rm)
	return c
}

func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			x[k] = normalizeYAML(vv)
		}
		return x
	case map[any]any:
		m := map[string]any{}
		for k, vv := range x {
			m[fmt.Sprint(k)] = normalizeYAML(vv)
		}
		return m
	case []any:
		for i := range x {
			x[i] = normalizeYAML(x[i])
		}
	}
	return v
}

// ---- datasets ------------------------------------------------------------------

func (a *app) datasetCmd() *cobra.Command {
	c := &cobra.Command{Use: "dataset", Aliases: []string{"datasets"}, Short: "Dataset registry (node paths + buckets)"}
	var name, version, mount, creds, dsFile string
	var locs []string
	var authoritative bool
	reg := &cobra.Command{
		Use:   "register --name <n> --version <v> --location ...",
		Short: "Register a dataset and where copies live",
		Example: `  mldojo dataset register --name pusht --version v1 --mount /data/pusht \
      --location node:gpu-a:/data/pusht --authoritative \
      --location bucket:shared-bucket/team_lab/users/bob/datasets/pusht`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := v1.Dataset{Name: name, Version: version, Mount: mount}
			if dsFile != "" {
				// {id|name, version, mount, locations: [{kind, node, path, provider, bucket, credentials, authoritative}]}
				b, err := os.ReadFile(dsFile)
				if err != nil {
					return usageErr("%v", err)
				}
				var m map[string]any
				if err := yaml.Unmarshal(b, &m); err != nil {
					return usageErr("%s: %v", dsFile, err)
				}
				if m["name"] == nil {
					m["name"] = m["id"]
				}
				jb, _ := json.Marshal(normalizeYAML(m))
				var fd v1.Dataset
				if err := json.Unmarshal(jb, &fd); err != nil {
					return usageErr("%s: %v", dsFile, err)
				}
				fd.ID = ""
				if !cmd.Flags().Changed("name") {
					d.Name = fd.Name
				}
				if !cmd.Flags().Changed("version") && fd.Version != "" {
					d.Version = fd.Version
				}
				if !cmd.Flags().Changed("mount") {
					d.Mount = fd.Mount
				}
				d.Locations = fd.Locations
			}
			for i, l := range locs {
				loc, err := datasets.ParseLocation(l, creds)
				if err != nil {
					return usageErr("%v", err)
				}
				if i == 0 && authoritative {
					loc.Authoritative = true
				}
				d.Locations = append(d.Locations, loc)
			}
			var out v1.Dataset
			if err := a.client().Do(cmd.Context(), "POST", "/datasets", d, &out); err != nil {
				return err
			}
			return a.out().Emit(out, func(w ioWriter) {
				fmt.Fprintf(w, "registered %s@%s (%d locations)\n", out.Name, out.Version, len(out.Locations))
			})
		},
	}
	f := reg.Flags()
	f.StringVar(&name, "name", "", "dataset name")
	f.StringVar(&version, "version", "v1", "version")
	f.StringVar(&mount, "mount", "", "default mount path inside runs (e.g. /data/pusht)")
	f.StringArrayVar(&locs, "location", nil, "node:<node>:<path> or bucket:<provider>/<bucket>/<path> (append ',authoritative' to mark one)")
	f.BoolVar(&authoritative, "authoritative", false, "mark the first --location as authoritative")
	f.StringVar(&creds, "bucket-credentials", "", "secret:// reference for bucket locations")
	f.StringVarP(&dsFile, "file", "f", "", "dataset definition YAML")
	ls := &cobra.Command{
		Use: "ls", Short: "List datasets", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ds []v1.Dataset
			if err := a.client().Get(cmd.Context(), "/datasets", &ds); err != nil {
				return err
			}
			return a.out().Emit(ds, func(w ioWriter) {
				rows := [][]string{}
				for _, d := range ds {
					var l []string
					for _, x := range d.Locations {
						l = append(l, datasets.FormatLocation(x))
					}
					rows = append(rows, []string{d.Name + "@" + d.Version, d.Mount, strings.Join(l, "  ")})
				}
				output.Table(w, []string{"DATASET", "MOUNT", "LOCATIONS"}, rows)
			})
		},
	}
	show := &cobra.Command{
		Use: "show <name@version>", Short: "Show a dataset", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var d v1.Dataset
			if err := a.client().Get(cmd.Context(), "/datasets/"+url.PathEscape(args[0]), &d); err != nil {
				return err
			}
			return a.out().Emit(d, func(w ioWriter) {
				output.KV(w, [2]string{"dataset", d.Name + "@" + d.Version}, [2]string{"mount", d.Mount})
				for _, l := range d.Locations {
					fmt.Fprintln(w, "  -", datasets.FormatLocation(l))
				}
			})
		},
	}
	var to string
	push := &cobra.Command{
		Use: "push <name@version> --to node:<id>", Short: "Pre-warm a node's dataset cache", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return usageErr("--to node:<id> is required")
			}
			var res map[string]string
			if err := a.client().Do(cmd.Context(), "POST", "/datasets/"+url.PathEscape(args[0])+"/push", map[string]string{"node": to}, &res); err != nil {
				return err
			}
			return a.out().Emit(res, func(w ioWriter) { fmt.Fprintf(w, "%s is at %s:%s\n", args[0], res["node"], res["path"]) })
		},
	}
	push.Flags().StringVar(&to, "to", "", "node:<id>")
	rm := &cobra.Command{
		Use: "rm <name@version>", Short: "Unregister a dataset (files are not deleted)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.client().Do(cmd.Context(), "DELETE", "/datasets/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			return a.out().Emit(map[string]bool{"ok": true}, func(w ioWriter) { fmt.Fprintf(w, "removed %s\n", args[0]) })
		},
	}
	c.AddCommand(reg, ls, show, push, rm)
	return c
}

// ---- secrets -------------------------------------------------------------------

func (a *app) secretCmd() *cobra.Command {
	c := &cobra.Command{Use: "secret", Aliases: []string{"secrets"}, Short: "Encrypted secrets (secret://namespace/name)"}
	var fromFile, desc string
	var stdin bool
	set := &cobra.Command{
		Use:   "set <namespace>/<name> (--from-file PATH | --stdin)",
		Short: "Store a secret (namespaces: ssh_keys, passwords, buckets, <plugin>, ...)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, name, ok := strings.Cut(strings.TrimPrefix(args[0], "secret://"), "/")
			if !ok || ns == "" || name == "" {
				return usageErr("expected <namespace>/<name>")
			}
			var val []byte
			var err error
			switch {
			case fromFile != "":
				val, err = os.ReadFile(config.ExpandHome(fromFile))
			case stdin:
				val, err = io.ReadAll(os.Stdin)
				if err == nil && !strings.Contains(strings.TrimRight(string(val), "\n"), "\n") {
					val = []byte(strings.TrimRight(string(val), "\r\n"))
				}
			default:
				return usageErr("--from-file or --stdin is required")
			}
			if err != nil {
				return usageErr("%v", err)
			}
			var m v1.SecretMeta
			body := map[string]string{"value_b64": base64.StdEncoding.EncodeToString(val), "description": desc}
			if err := a.client().Do(cmd.Context(), "POST", "/secrets/"+url.PathEscape(ns)+"/"+url.PathEscape(name), body, &m); err != nil {
				return err
			}
			return a.out().Emit(m, func(w ioWriter) { fmt.Fprintf(w, "stored %s (%d bytes)\n", m.Ref, m.Size) })
		},
	}
	set.Flags().StringVar(&fromFile, "from-file", "", "read the value from a file")
	set.Flags().BoolVar(&stdin, "stdin", false, "read the value from stdin")
	set.Flags().StringVar(&desc, "description", "", "description")
	ls := &cobra.Command{
		Use: "ls", Short: "List secrets (names only)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var l []v1.SecretMeta
			if err := a.client().Get(cmd.Context(), "/secrets", &l); err != nil {
				return err
			}
			var st v1.SecretStatus
			a.client().Get(cmd.Context(), "/secrets/status", &st)
			return a.out().Emit(l, func(w ioWriter) {
				state := "locked"
				if st.Unlocked {
					state = "unlocked (" + st.Source + ")"
				}
				fmt.Fprintf(w, "master key: %s\n", state)
				rows := [][]string{}
				for _, s := range l {
					rows = append(rows, []string{s.Ref, strconv.Itoa(s.Size), output.Ago(&s.UpdatedAt), s.Description})
				}
				output.Table(w, []string{"REF", "BYTES", "UPDATED", "DESCRIPTION"}, rows)
			})
		},
	}
	rm := &cobra.Command{
		Use: "rm <namespace>/<name>", Short: "Delete a secret", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, name, ok := strings.Cut(strings.TrimPrefix(args[0], "secret://"), "/")
			if !ok {
				return usageErr("expected <namespace>/<name>")
			}
			if err := a.client().Do(cmd.Context(), "DELETE", "/secrets/"+url.PathEscape(ns)+"/"+url.PathEscape(name), nil, nil); err != nil {
				return err
			}
			return a.out().Emit(map[string]bool{"ok": true}, func(w ioWriter) { fmt.Fprintf(w, "deleted secret://%s/%s\n", ns, name) })
		},
	}
	var keyFile string
	unlock := &cobra.Command{
		Use:   "unlock",
		Short: "Unlock the server's secrets with the master key (keychain first, then MLDOJO_MASTER_KEY)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, source := "", ""
			if keyFile != "" {
				b, err := os.ReadFile(config.ExpandHome(keyFile))
				if err != nil {
					return usageErr("%v", err)
				}
				key, source = strings.TrimSpace(string(b)), keyFile
			} else if k, err := keychain.Get(); err == nil && k != "" {
				key, source = k, "keychain"
			} else if k := os.Getenv("MLDOJO_MASTER_KEY"); k != "" {
				key, source = k, "MLDOJO_MASTER_KEY"
			}
			var st v1.SecretStatus
			if err := a.client().Do(cmd.Context(), "POST", "/secrets/unlock", map[string]string{"key": key}, &st); err != nil {
				return err
			}
			if source == "" {
				source = "server-side keychain/env/file"
			}
			return a.out().Emit(st, func(w ioWriter) { fmt.Fprintf(w, "secrets unlocked (key from %s)\n", source) })
		},
	}
	unlock.Flags().StringVar(&keyFile, "key-file", "", "read the age master key from a file")
	c.AddCommand(set, ls, rm, unlock)
	return c
}

// ---- AI --------------------------------------------------------------------------

func (a *app) aiCmd() *cobra.Command {
	c := &cobra.Command{Use: "ai", Short: "AI-friendly compact endpoints"}
	jsonCall := func(use, short string, nargs int, method string, path func(args []string) (string, error), body func(args []string) (any, error)) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(nargs),
			RunE: func(cmd *cobra.Command, args []string) error {
				p, err := path(args)
				if err != nil {
					return err
				}
				var in any
				if body != nil {
					if in, err = body(args); err != nil {
						return err
					}
				}
				var out json.RawMessage
				if err := a.client().Do(cmd.Context(), method, p, in, &out); err != nil {
					return err
				}
				var v any
				json.Unmarshal(out, &v)
				return a.out().Emit(v, func(w ioWriter) {
					if m, ok := v.(map[string]any); ok && m["summary"] != nil {
						fmt.Fprintln(w, m["summary"])
						fmt.Fprintln(w)
					}
					b, _ := json.MarshalIndent(v, "", "  ")
					fmt.Fprintln(w, string(b))
				})
			},
		}
	}
	c.AddCommand(
		jsonCall("brief <run-id>", "One-shot run summary: status, progress, latest metrics, anomalies, best ckpt", 1, "GET",
			func(a []string) (string, error) { return "/ai/runs/" + url.PathEscape(a[0]) + "/brief", nil }, nil),
		a.freeNodesCmd(jsonCall),
		jsonCall("summarize-exp <project>/<exp>", "Experiment summary, ranking and next steps", 1, "GET",
			func(a []string) (string, error) {
				p, e, err := splitExp(a[0])
				return "/ai/experiments/" + url.PathEscape(p) + "/" + url.PathEscape(e) + "/summary", err
			}, nil),
		jsonCall("anomaly-check <run-id>", "Scan a run for NaNs, spikes, stalls, OOMs", 1, "POST",
			func(a []string) (string, error) { return "/ai/anomaly-check/" + url.PathEscape(a[0]), nil },
			func([]string) (any, error) { return map[string]any{}, nil }),
		jsonCall("submit <json>", `Loose submit: '{"project":"x","target":"node:gpu-a","cmd":"python train.py","seeds":[0,1]}'`, 1, "POST",
			func([]string) (string, error) { return "/ai/runs", nil },
			func(a []string) (any, error) {
				raw := a[0]
				if b, err := os.ReadFile(raw); err == nil {
					raw = string(b)
				}
				var v map[string]any
				if err := json.Unmarshal([]byte(raw), &v); err != nil {
					return nil, usageErr("ai submit expects a JSON object (or a file containing one): %v", err)
				}
				return v, nil
			}),
	)
	return c
}

// ---- dev -------------------------------------------------------------------------

// devPostgresPassword returns $POSTGRES_PASSWORD, else a random password kept
// in <config dir>/dev-postgres-password so the compose volume stays usable
// across restarts. There is no fixed default password.
func devPostgresPassword() (string, error) {
	if pw := os.Getenv("POSTGRES_PASSWORD"); pw != "" {
		return pw, nil
	}
	f := filepath.Join(config.Dir(), "dev-postgres-password")
	if b, err := os.ReadFile(f); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	pw := hex.EncodeToString(buf)
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return "", err
	}
	return pw, os.WriteFile(f, []byte(pw+"\n"), 0o600)
}

func (a *app) devCmd() *cobra.Command {
	c := &cobra.Command{Use: "dev", Short: "Local development helpers"}
	var web bool
	up := &cobra.Command{
		Use:   "up",
		Short: "Run postgres (docker compose, unless MLDOJO_DATABASE_URL is set), the API and the web dev server in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			env := os.Environ()
			if os.Getenv("MLDOJO_DATABASE_URL") == "" {
				pw, err := devPostgresPassword()
				if err != nil {
					return err
				}
				fmt.Fprintln(os.Stderr, "starting postgres via docker compose...")
				dc := exec.CommandContext(ctx, "docker", "compose", "up", "-d", "postgres")
				dc.Dir, dc.Stdout, dc.Stderr = root, os.Stderr, os.Stderr
				dc.Env = append(os.Environ(), "POSTGRES_PASSWORD="+pw)
				if err := dc.Run(); err != nil {
					return usageErr("docker compose failed (%v); set MLDOJO_DATABASE_URL to an existing postgres instead", err)
				}
				env = append(env, "MLDOJO_DATABASE_URL=postgres://mldojo:"+pw+"@127.0.0.1:5432/mldojo?sslmode=disable")
			}
			procs := []*exec.Cmd{exec.CommandContext(ctx, "go", "run", "./api/cmd/mldojo-api")}
			if web {
				p := exec.CommandContext(ctx, "npm", "run", "dev")
				p.Dir = filepath.Join(root, "web")
				p.Env = append(os.Environ(), "NEXT_PUBLIC_API_BASE=http://127.0.0.1:8765")
				procs = append(procs, p)
			}
			for _, p := range procs {
				if p.Dir == "" {
					p.Dir = root
				}
				if p.Env == nil {
					p.Env = env
				}
				p.Stdout, p.Stderr = os.Stdout, os.Stderr
				if err := p.Start(); err != nil {
					return err
				}
			}
			fmt.Fprintln(os.Stderr, "api: http://127.0.0.1:8765  web: http://127.0.0.1:3000  (ctrl-c to stop)")
			for _, p := range procs {
				p.Wait()
			}
			return nil
		},
	}
	up.Flags().BoolVar(&web, "web", true, "also run the Next.js dev server")
	c.AddCommand(up)
	return c
}

func repoRoot() (string, error) {
	dir, _ := os.Getwd()
	for d := dir; ; d = filepath.Dir(d) {
		if fileExists(filepath.Join(d, "go.mod")) && fileExists(filepath.Join(d, "docker-compose.yml")) {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", usageErr("run `mldojo dev up` inside the mldojo repository")
		}
	}
}

// freeNodesCmd wraps /ai/nodes/free with the filters an agent actually has:
// how many cards it needs and how much memory each must have free. Without
// them the caller fetches every node and re-implements the decision.
func (a *app) freeNodesCmd(jsonCall func(use, short string, nargs int, method string,
	path func([]string) (string, error), body func([]string) (any, error)) *cobra.Command) *cobra.Command {
	var gpus int
	var minFree float64
	var labels string
	c := jsonCall("free-nodes", "Online nodes with GPUs something could run on", 0, "GET",
		func([]string) (string, error) {
			q := url.Values{}
			if gpus > 0 {
				q.Set("gpus", strconv.Itoa(gpus))
			}
			if minFree > 0 {
				q.Set("min_free_gb", strconv.FormatFloat(minFree, 'f', -1, 64))
			}
			if labels != "" {
				q.Set("labels", labels)
			}
			if len(q) == 0 {
				return "/ai/nodes/free", nil
			}
			return "/ai/nodes/free?" + q.Encode(), nil
		}, nil)
	c.Flags().IntVar(&gpus, "gpus", 0, "only nodes with at least this many usable GPUs")
	c.Flags().Float64Var(&minFree, "min-free-gb", 0, "a GPU counts as usable only with this much free memory")
	c.Flags().StringVar(&labels, "label", "", "comma separated labels the node must carry")
	return c
}

// gpuSeries mirrors the API's history payload.
type gpuSeries struct {
	GPUIndex int                 `json:"gpu_index"`
	Points   []v1.GPUSamplePoint `json:"points"`
}

// printGPUHistory draws each card as a sparkline. A table of 500 points per
// card is unreadable; the shape is the whole point -- a stalled dataloader,
// a card held at zero, a job that died an hour ago.
func printGPUHistory(w io.Writer, h []gpuSeries) {
	if len(h) == 0 {
		fmt.Fprintln(w, "no samples in this window")
		return
	}
	for _, s := range h {
		var util, mem []float64
		for _, p := range s.Points {
			util = append(util, p.Util)
			if p.MemTotMB > 0 {
				mem = append(mem, p.MemUsedMB/p.MemTotMB*100)
			}
		}
		last := s.Points[len(s.Points)-1]
		fmt.Fprintf(w, "GPU %d  %s .. %s  (%d points)\n", s.GPUIndex,
			s.Points[0].TS.Local().Format("01-02 15:04"), last.TS.Local().Format("01-02 15:04"), len(s.Points))
		fmt.Fprintf(w, "  util %s  avg %.0f%%  max %.0f%%\n", sparkline(util), mean(util), maxOf(util))
		fmt.Fprintf(w, "  mem  %s  last %.0f/%.0f MiB\n", sparkline(mem), last.MemUsedMB, last.MemTotMB)
	}
}

var sparks = []rune("▁▂▃▄▅▆▇█")

// sparkline scales to 0..100, because that is what both series are: these
// are percentages, and a self-scaling chart would make an idle card look busy.
func sparkline(v []float64) string {
	if len(v) == 0 {
		return ""
	}
	const width = 60
	out := make([]rune, 0, width)
	step := float64(len(v)) / width
	if step < 1 {
		step = 1
	}
	for i := 0.0; int(i) < len(v); i += step {
		x := v[int(i)]
		idx := int(x / 100 * float64(len(sparks)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparks) {
			idx = len(sparks) - 1
		}
		out = append(out, sparks[idx])
	}
	return string(out)
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}

func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

type nodeDiskResp struct {
	Node  string       `json:"node"`
	Usage v1.DiskUsage `json:"usage"`
	Runs  map[string]struct {
		Project    string `json:"project"`
		Experiment string `json:"experiment"`
		Name       string `json:"name"`
		Status     string `json:"status"`
	} `json:"runs"`
}
