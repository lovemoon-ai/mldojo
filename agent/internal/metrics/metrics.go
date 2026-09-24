// Package metrics scans metric files incrementally: JSON lines and TensorBoard event
// files. Scanners remember byte offsets so each call returns only new points.
package metrics

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"google.golang.org/protobuf/encoding/protowire"
)

type Scanner struct {
	offsets map[string]int64
	lines   map[string]int64
}

func NewScanner() *Scanner {
	return &Scanner{offsets: map[string]int64{}, lines: map[string]int64{}}
}

// Scan reads every source (paths relative to root) and returns new points.
func (s *Scanner) Scan(root string, sources []v1.MetricsSource) []v1.MetricPoint {
	var out []v1.MetricPoint
	for _, src := range sources {
		p := src.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		switch src.Type {
		case "jsonl":
			files, _ := filepath.Glob(p)
			if len(files) == 0 {
				files = []string{p}
			}
			for _, f := range files {
				out = append(out, s.ScanJSONL(f)...)
			}
		case "tensorboard":
			out = append(out, s.ScanTensorboard(p)...)
		}
	}
	return out
}

func readFrom(path string, off int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < off {
		return nil, io.ErrUnexpectedEOF // truncated/rotated
	}
	if st.Size() == off {
		return nil, nil
	}
	n := st.Size() - off
	if n > 64<<20 {
		n = 64 << 20
	}
	buf := make([]byte, n)
	_, err = f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

var nanRe = regexp.MustCompile(`([:\[,]\s*)(-?Infinity|NaN)(\s*[,}\]])`)

var stepKeys = []string{"step", "_step", "global_step", "iteration", "iter", "epoch"}
var tsKeys = map[string]bool{"_timestamp": true, "timestamp": true, "time": true, "ts": true, "_runtime": true, "wall_time": true}

// ScanJSONL parses complete new lines of a JSON-lines metrics file.
func (s *Scanner) ScanJSONL(path string) []v1.MetricPoint {
	off := s.offsets[path]
	data, err := readFrom(path, off)
	if err == io.ErrUnexpectedEOF {
		s.offsets[path], s.lines[path] = 0, 0
		off = 0
		data, err = readFrom(path, 0)
	}
	if err != nil || len(data) == 0 {
		return nil
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil
	}
	var out []v1.MetricPoint
	for _, line := range bytes.Split(data[:end], []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		s.lines[path]++
		out = append(out, ParseLine(line, s.lines[path]-1)...)
	}
	s.offsets[path] = off + int64(end) + 1
	return out
}

// ParseLine turns one JSON object into points. lineNo is the fallback step.
func ParseLine(line []byte, lineNo int64) []v1.MetricPoint {
	line = nanRe.ReplaceAll(line, []byte(`$1"$2"$3`))
	line = nanRe.ReplaceAll(line, []byte(`$1"$2"$3`)) // adjacent matches
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return nil
	}
	step := lineNo
	for _, k := range stepKeys {
		if v, ok := toFloat(obj[k]); ok {
			step = int64(v)
			break
		}
	}
	ts := time.Now()
	for k := range tsKeys {
		if v, ok := toFloat(obj[k]); ok && v > 1e9 {
			sec, frac := math.Modf(v)
			ts = time.Unix(int64(sec), int64(frac*1e9))
			break
		}
	}
	flat := map[string]float64{}
	flatten("", obj, flat)
	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]v1.MetricPoint, 0, len(keys))
	for _, k := range keys {
		out = append(out, v1.MetricPoint{Step: step, Key: k, Value: flat[k], TS: ts})
	}
	return out
}

func flatten(prefix string, v map[string]any, out map[string]float64) {
	for k, x := range v {
		if prefix == "" {
			skip := tsKeys[k]
			for _, sk := range stepKeys {
				if k == sk {
					skip = true
				}
			}
			if skip {
				continue
			}
		}
		key := k
		if prefix != "" {
			key = prefix + "/" + k
		}
		if m, ok := x.(map[string]any); ok {
			flatten(key, m, out)
			continue
		}
		if f, ok := toFloat(x); ok {
			out[key] = f
		}
	}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		switch x {
		case "NaN":
			return math.NaN(), true
		case "Infinity":
			return math.Inf(1), true
		case "-Infinity":
			return math.Inf(-1), true
		}
	}
	return 0, false
}

// ScanTensorboard reads new scalar events from event files under path.
func (s *Scanner) ScanTensorboard(path string) []v1.MetricPoint {
	var files []string
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		files = []string{path}
	} else {
		filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.Contains(d.Name(), "tfevents") {
				files = append(files, p)
			}
			return nil
		})
	}
	sort.Strings(files)
	var out []v1.MetricPoint
	for _, f := range files {
		// Nested run dirs (e.g. tb/train, tb/val) prefix their tags.
		prefix := ""
		if rel, err := filepath.Rel(path, filepath.Dir(f)); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			prefix = filepath.ToSlash(rel) + "/"
		}
		off := s.offsets[f]
		data, err := readFrom(f, off)
		if err != nil || len(data) == 0 {
			continue
		}
		pos := 0
		for len(data)-pos >= 12 {
			n := int(binary.LittleEndian.Uint64(data[pos:]))
			if n < 0 || len(data)-pos < 12+n+4 {
				break // partial record: wait for more
			}
			rec := data[pos+12 : pos+12+n]
			pos += 12 + n + 4
			for _, p := range parseEvent(rec) {
				p.Key = prefix + p.Key
				out = append(out, p)
			}
		}
		s.offsets[f] = off + int64(pos)
	}
	return out
}

func parseEvent(b []byte) []v1.MetricPoint {
	var wall float64
	var step int64
	var summaries [][]byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.Fixed64Type:
			v, n := protowire.ConsumeFixed64(b)
			if n < 0 {
				return nil
			}
			wall, b = math.Float64frombits(v), b[n:]
		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil
			}
			step, b = int64(v), b[n:]
		case num == 5 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil
			}
			summaries, b = append(summaries, v), b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil
			}
			b = b[n:]
		}
	}
	ts := time.Now()
	if wall > 0 {
		sec, frac := math.Modf(wall)
		ts = time.Unix(int64(sec), int64(frac*1e9))
	}
	var out []v1.MetricPoint
	for _, sum := range summaries {
		for len(sum) > 0 {
			num, typ, n := protowire.ConsumeTag(sum)
			if n < 0 {
				break
			}
			sum = sum[n:]
			if num == 1 && typ == protowire.BytesType {
				v, n := protowire.ConsumeBytes(sum)
				if n < 0 {
					break
				}
				sum = sum[n:]
				if tag, val, ok := parseValue(v); ok {
					out = append(out, v1.MetricPoint{Step: step, Key: tag, Value: val, TS: ts})
				}
				continue
			}
			n = protowire.ConsumeFieldValue(num, typ, sum)
			if n < 0 {
				break
			}
			sum = sum[n:]
		}
	}
	return out
}

// parseValue extracts (tag, scalar) from a Summary.Value.
func parseValue(b []byte) (string, float64, bool) {
	var tag string
	var val float64
	found := false
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return "", 0, false
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return "", 0, false
			}
			tag, b = string(v), b[n:]
		case num == 2 && typ == protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(b)
			if n < 0 {
				return "", 0, false
			}
			val, found, b = float64(math.Float32frombits(v)), true, b[n:]
		case num == 8 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return "", 0, false
			}
			b = b[n:]
			if f, ok := tensorScalar(v); ok {
				val, found = f, true
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return "", 0, false
			}
			b = b[n:]
		}
	}
	return tag, val, found && tag != ""
}

// tensorScalar reads a single-element float/double/int TensorProto.
func tensorScalar(b []byte) (float64, bool) {
	var dtype uint64
	var vals []float64
	var content []byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0, false
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return 0, false
			}
			dtype, b = v, b[n:]
		case num == 4 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return 0, false
			}
			content, b = v, b[n:]
		case num == 5 && typ == protowire.BytesType: // packed float_val
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return 0, false
			}
			for i := 0; i+4 <= len(v); i += 4 {
				vals = append(vals, float64(math.Float32frombits(binary.LittleEndian.Uint32(v[i:]))))
			}
			b = b[n:]
		case num == 5 && typ == protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(b)
			if n < 0 {
				return 0, false
			}
			vals, b = append(vals, float64(math.Float32frombits(v))), b[n:]
		case num == 6 && typ == protowire.BytesType: // packed double_val
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return 0, false
			}
			for i := 0; i+8 <= len(v); i += 8 {
				vals = append(vals, math.Float64frombits(binary.LittleEndian.Uint64(v[i:])))
			}
			b = b[n:]
		case (num == 7 || num == 10) && typ == protowire.BytesType: // packed int_val / int64_val
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return 0, false
			}
			for len(v) > 0 {
				x, m := protowire.ConsumeVarint(v)
				if m < 0 {
					break
				}
				vals, v = append(vals, float64(int64(x))), v[m:]
			}
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return 0, false
			}
			b = b[n:]
		}
	}
	if len(vals) == 1 {
		return vals[0], true
	}
	if len(vals) == 0 && len(content) > 0 {
		switch {
		case dtype == 1 && len(content) == 4:
			return float64(math.Float32frombits(binary.LittleEndian.Uint32(content))), true
		case dtype == 2 && len(content) == 8:
			return math.Float64frombits(binary.LittleEndian.Uint64(content)), true
		}
	}
	return 0, false
}
