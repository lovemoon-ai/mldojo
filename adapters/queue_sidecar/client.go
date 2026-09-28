// Package queue_sidecar is the Go side of a queue plugin: an HTTP client for
// the sidecar process that talks to an external scheduler (protocol:
// docs/queue-plugins.md; reference mock: ./mock).
package queue_sidecar

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(base string) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 120 * time.Second}}
}

type Mount struct {
	Bucket string `json:"bucket"`
	Path   string `json:"path"`
	Mount  string `json:"mount"`
}

// SubmitRequest mirrors the sidecar POST /jobs body.
type SubmitRequest struct {
	RunID        string            `json:"run_id"`
	JobName      string            `json:"job_name"`
	QueueName    string            `json:"queue_name"`
	ProjectID    string            `json:"project_id"`
	DockerImage  string            `json:"docker_image"`
	NumWorkers   int               `json:"num_workers"`
	GPUPerWorker int               `json:"gpu_per_worker"`
	CPUPerWorker int               `json:"cpu_per_worker"`
	CPUMemRatio  int               `json:"cpu_mem_ratio"`
	WallTimeMin  int               `json:"wall_time_min"`
	Cmd          string            `json:"cmd"`
	Workdir      string            `json:"workdir"`
	BundleURL    string            `json:"bundle_url"`
	BundleToken  string            `json:"bundle_token"`
	Env          map[string]string `json:"env"`
	InputBucket  string            `json:"input_bucket"`
	OutputBucket string            `json:"output_bucket"`
	Mounts       []Mount           `json:"mounts"`
	JobPassword  string            `json:"job_password"`
	Credentials  string            `json:"credentials,omitempty"`
	Extra        map[string]any    `json:"extra,omitempty"`
}

type SubmitResponse struct {
	JobID           string          `json:"job_id"`
	WorkspaceFolder string          `json:"workspace_folder"`
	URL             string          `json:"url"`
	DagID           *string         `json:"dag_id"`
	Raw             json.RawMessage `json:"raw,omitempty"`
}

type Status struct {
	JobID      string     `json:"job_id"`
	Phase      string     `json:"phase"`
	RawPhase   string     `json:"raw_phase"`
	Message    string     `json:"message"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	ExitCode   *int       `json:"exit_code"`
}

type MetricPath struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

type Health struct {
	OK    bool    `json:"ok"`
	Mock  bool    `json:"mock"`
	SDK   *string `json:"sdk"`
	Error string  `json:"error,omitempty"`
}

// HTTPError is a non-2xx answer from the sidecar.
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("queue plugin: %s (HTTP %d)", e.Msg, e.Status) }

func (c *Client) do(ctx context.Context, method, path, creds string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if creds != "" {
		req.Header.Set("X-Queue-Credentials", base64.StdEncoding.EncodeToString([]byte(creds)))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("queue plugin unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &HTTPError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) Health(ctx context.Context) (*Health, error) {
	var h Health
	return &h, c.do(ctx, "GET", "/health", "", nil, &h)
}

func (c *Client) Submit(ctx context.Context, r *SubmitRequest) (*SubmitResponse, error) {
	var out SubmitResponse
	if err := c.do(ctx, "POST", "/jobs", r.Credentials, r, &out); err != nil {
		return nil, err
	}
	if out.JobID == "" {
		return nil, fmt.Errorf("queue plugin returned no job_id")
	}
	return &out, nil
}

func (c *Client) Status(ctx context.Context, jobID, creds string) (*Status, error) {
	var s Status
	return &s, c.do(ctx, "GET", "/jobs/"+url.PathEscape(jobID)+"/status", creds, nil, &s)
}

func (c *Client) Log(ctx context.Context, jobID, creds string) (string, error) {
	var l struct {
		Log string `json:"log"`
	}
	err := c.do(ctx, "GET", "/jobs/"+url.PathEscape(jobID)+"/log", creds, nil, &l)
	return l.Log, err
}

func (c *Client) Cancel(ctx context.Context, jobID, creds string) error {
	return c.do(ctx, "POST", "/jobs/"+url.PathEscape(jobID)+"/cancel", creds, map[string]any{}, nil)
}

// FileGlob selects job output files of one artifact kind.
type FileGlob struct {
	Kind string `json:"kind"`
	Glob string `json:"glob"`
}

// File is a job output file (path relative to /job_data or the bucket).
type File struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Files lists job outputs matching globs.
func (c *Client) Files(ctx context.Context, jobID, creds, bucket string, globs []FileGlob) ([]File, error) {
	var out struct {
		Files []File `json:"files"`
	}
	err := c.do(ctx, "POST", "/jobs/"+url.PathEscape(jobID)+"/files", creds,
		map[string]any{"bucket": bucket, "globs": globs, "max_files": 2000}, &out)
	return out.Files, err
}

// Download streams one job output file into w.
func (c *Client) Download(ctx context.Context, jobID, creds, bucket, path string, w io.Writer) (int64, error) {
	q := url.Values{"bucket": {bucket}, "path": {path}}
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/jobs/"+url.PathEscape(jobID)+"/download?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	if creds != "" {
		req.Header.Set("X-Queue-Credentials", base64.StdEncoding.EncodeToString([]byte(creds)))
	}
	hc := *c.HTTP
	hc.Timeout = 0 // large files; ctx bounds the transfer
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("queue plugin unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(b, &e)
		return 0, &HTTPError{Status: resp.StatusCode, Msg: e.Error}
	}
	return io.Copy(w, resp.Body)
}

func (c *Client) Metrics(ctx context.Context, jobID, creds, bucket string, paths []MetricPath, tracking bool) ([]v1.MetricPoint, error) {
	var out struct {
		Points []v1.MetricPoint `json:"points"`
	}
	err := c.do(ctx, "POST", "/jobs/"+url.PathEscape(jobID)+"/metrics", creds,
		map[string]any{"bucket": bucket, "paths": paths, "tracking": tracking}, &out)
	return out.Points, err
}

// QueueResource is one scheduler queue's live capacity (GET /resources).
// Total/Used/Free count Unit ("gpu" or "cpu"); Utilization is 0..1 or nil.
type QueueResource struct {
	Name          string   `json:"name"`
	Cluster       string   `json:"cluster"`
	Accelerator   string   `json:"accelerator"`
	Unit          string   `json:"unit"`
	Total         float64  `json:"total"`
	Used          float64  `json:"used"`
	Free          float64  `json:"free"`
	RunningJobs   int      `json:"running_jobs"`
	QueuedJobs    int      `json:"queued_jobs"`
	QueuedWaitSec int      `json:"queued_wait_sec"`
	Usable        bool     `json:"usable"`
	Utilization   *float64 `json:"utilization"`
}

// Resources returns the scheduler's queue capacity; plugins without it answer 404/501.
func (c *Client) Resources(ctx context.Context, creds string) ([]QueueResource, []string, error) {
	var out struct {
		Queues   []QueueResource `json:"queues"`
		Warnings []string        `json:"warnings"`
	}
	err := c.do(ctx, "GET", "/resources", creds, nil, &out)
	return out.Queues, out.Warnings, err
}
