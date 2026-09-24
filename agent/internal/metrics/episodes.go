package metrics

import (
	"encoding/csv"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Episodes reads a run's evaluation results from whatever the harness left
// behind. Three shapes cover what real harnesses write, and none of them
// requires touching the harness:
//
//   - csv       a manifest with one row per episode
//   - jsonl     one JSON object per episode (what the SDK writes)
//   - filenames a directory of episode7_success.mp4 / episode8_failure.mp4
//
// Episodes are re-read whole each time rather than incrementally: an
// evaluation produces tens of rows, not millions, and a harness that
// rewrites its manifest at the end must not be missed.
func (s *Scanner) Episodes(root string, sources []v1.EpisodesSource) []v1.Episode {
	byIdx := map[int]v1.Episode{}
	for _, src := range sources {
		p := src.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		for _, e := range readEpisodes(src.Type, p) {
			// Later sources fill gaps in earlier ones: a manifest gives the
			// seed, the directory gives the video.
			cur, ok := byIdx[e.Index]
			if !ok {
				byIdx[e.Index] = e
				continue
			}
			byIdx[e.Index] = mergeEpisode(cur, e)
		}
	}
	out := make([]v1.Episode, 0, len(byIdx))
	for _, e := range byIdx {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

func mergeEpisode(a, b v1.Episode) v1.Episode {
	if a.Seed == nil {
		a.Seed = b.Seed
	}
	if a.Steps == 0 {
		a.Steps = b.Steps
	}
	if a.DurationMS == 0 {
		a.DurationMS = b.DurationMS
	}
	if a.VideoURI == "" {
		a.VideoURI = b.VideoURI
	}
	for k, v := range b.Extra {
		if a.Extra == nil {
			a.Extra = map[string]any{}
		}
		if _, ok := a.Extra[k]; !ok {
			a.Extra[k] = v
		}
	}
	return a
}

func readEpisodes(typ, path string) []v1.Episode {
	if typ == "" {
		typ = guessEpisodeType(path)
	}
	// A harness that writes a fresh timestamped directory per run can only
	// be named with a glob, so every source accepts one.
	paths := []string{path}
	if strings.ContainsAny(path, "*?[") && typ != "filenames" {
		m, err := filepath.Glob(path)
		if err != nil || len(m) == 0 {
			return nil
		}
		sort.Strings(m)
		paths = m
	}
	var out []v1.Episode
	for _, p := range paths {
		switch typ {
		case "csv":
			out = append(out, readEpisodeCSV(p)...)
		case "jsonl":
			out = append(out, readEpisodeJSONL(p)...)
		case "filenames":
			out = append(out, readEpisodeFilenames(p)...)
		}
	}
	return out
}

func guessEpisodeType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		return "csv"
	case ".jsonl", ".json":
		return "jsonl"
	case "":
		return "filenames"
	}
	// A glob like episodes/*.mp4 is a directory of results.
	if strings.ContainsAny(path, "*?") {
		return "filenames"
	}
	return ""
}

// episodeFile matches episode7_success.mp4 and episode12_failure.mp4, the
// naming an evaluation harness reaches for when it starts saving video.
var episodeFile = regexp.MustCompile(`(?i)episode[_-]?(\d+)[_-](success|failure|fail|ok)\.(mp4|webm|mov|gif)$`)

func readEpisodeFilenames(path string) []v1.Episode {
	if strings.ContainsAny(path, "*?[") {
		m, err := filepath.Glob(path)
		if err != nil {
			return nil
		}
		sort.Strings(m)
		var out []v1.Episode
		for _, p := range m {
			out = append(out, walkEpisodeFiles(p)...)
		}
		return out
	}
	return walkEpisodeFiles(path)
}

func walkEpisodeFiles(root string) []v1.Episode {
	var out []v1.Episode
	n := 0
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if n++; n > 20000 {
			return filepath.SkipAll
		}
		m := episodeFile.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil {
			return nil
		}
		ok := strings.EqualFold(m[2], "success") || strings.EqualFold(m[2], "ok")
		out = append(out, v1.Episode{Index: idx, Success: ok, VideoURI: p})
		return nil
	})
	return out
}

// resolveVideo turns a manifest's filename into a path that exists, or into
// nothing. Pointing at a file that is not there is worse than admitting we
// do not know where it is: the episode looks playable and then is not.
func resolveVideo(dir, name string) string {
	if filepath.IsAbs(name) {
		if fileExists(name) {
			return name
		}
		return ""
	}
	if p := filepath.Join(dir, name); fileExists(p) {
		return p
	}
	// Harnesses commonly write the videos to a subdirectory of the run
	// rather than beside the manifest -- the one this was built against
	// uses episodes/<task>/. Look for the name instead of guessing.
	var found string
	base := filepath.Base(name)
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != base {
			return nil
		}
		found = p
		return filepath.SkipAll
	})
	return found
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// truthy accepts what harnesses actually write for "it worked".
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "t", "yes", "y", "success", "succeeded", "ok", "pass", "passed":
		return true
	}
	return false
}

// episodeAliases maps the column names harnesses use onto our fields. The
// point of this table is that the harness does not have to change.
var episodeAliases = map[string]string{
	"episode": "index", "idx": "index", "ep": "index", "i": "index", "index": "index",
	"seed":    "seed",
	"success": "success", "result": "success", "video_result": "success",
	"status": "success", "outcome": "success", "passed": "success",
	"steps": "steps", "step": "steps", "frames": "steps", "length": "steps",
	"duration_seconds": "duration_s", "duration_s": "duration_s", "seconds": "duration_s",
	"duration_ms": "duration_ms",
	"video":       "video", "filename": "video", "video_path": "video", "file": "video",
}

func readEpisodeCSV(path string) []v1.Episode {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	rows, err := r.ReadAll()
	if err != nil || len(rows) < 2 {
		return nil
	}
	cols := map[string]int{}
	for i, h := range rows[0] {
		if k, ok := episodeAliases[strings.ToLower(strings.TrimSpace(h))]; ok {
			if _, taken := cols[k]; !taken {
				cols[k] = i
			}
		}
	}
	if _, ok := cols["success"]; !ok {
		return nil
	}
	dir := filepath.Dir(path)
	var out []v1.Episode
	for n, row := range rows[1:] {
		get := func(k string) string {
			if i, ok := cols[k]; ok && i < len(row) {
				return strings.TrimSpace(row[i])
			}
			return ""
		}
		e := v1.Episode{Index: n, Success: truthy(get("success"))}
		if v, err := strconv.Atoi(get("index")); err == nil {
			e.Index = v
		}
		if v, err := strconv.ParseInt(get("seed"), 10, 64); err == nil {
			e.Seed = &v
		}
		if v, err := strconv.Atoi(get("steps")); err == nil {
			e.Steps = v
		}
		if v, err := strconv.ParseFloat(get("duration_s"), 64); err == nil {
			e.DurationMS = int64(v * 1000)
		} else if v, err := strconv.ParseInt(get("duration_ms"), 10, 64); err == nil {
			e.DurationMS = v
		}
		if v := get("video"); v != "" {
			e.VideoURI = resolveVideo(dir, v)
		}
		out = append(out, e)
	}
	return out
}

func readEpisodeJSONL(path string) []v1.Episode {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	dir := filepath.Dir(path)
	var out []v1.Episode
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		e := v1.Episode{Index: len(out), Extra: map[string]any{}}
		for k, v := range m {
			switch episodeAliases[strings.ToLower(k)] {
			case "index":
				if f, ok := toFloat(v); ok {
					e.Index = int(f)
				}
			case "seed":
				if f, ok := toFloat(v); ok {
					s := int64(f)
					e.Seed = &s
				}
			case "success":
				switch x := v.(type) {
				case bool:
					e.Success = x
				case string:
					e.Success = truthy(x)
				default:
					if f, ok := toFloat(v); ok {
						e.Success = f != 0
					}
				}
			case "steps":
				if f, ok := toFloat(v); ok {
					e.Steps = int(f)
				}
			case "duration_s":
				if f, ok := toFloat(v); ok {
					e.DurationMS = int64(f * 1000)
				}
			case "duration_ms":
				if f, ok := toFloat(v); ok {
					e.DurationMS = int64(f)
				}
			case "video":
				if s, ok := v.(string); ok {
					e.VideoURI = resolveVideo(dir, s)
				}
			default:
				e.Extra[k] = v
			}
		}
		if len(e.Extra) == 0 {
			e.Extra = nil
		}
		_ = n
		out = append(out, e)
	}
	return out
}
