package metrics

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestJSONLIncremental(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.jsonl")
	os.WriteFile(p, []byte(`{"step": 1, "loss": 0.5, "train": {"acc": 0.9}}`+"\n"+`{"step": 2, "loss": NaN}`+"\n"+`{"step": 3, "lo`), 0o644)
	s := NewScanner()
	pts := s.ScanJSONL(p)
	if len(pts) != 3 {
		t.Fatalf("got %d points: %+v", len(pts), pts)
	}
	if pts[0].Key != "loss" || pts[1].Key != "train/acc" || !math.IsNaN(pts[2].Value) || pts[2].Step != 2 {
		t.Fatalf("points: %+v", pts)
	}
	// complete the partial line
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`ss": 0.25}` + "\n")
	f.Close()
	pts = s.ScanJSONL(p)
	if len(pts) != 1 || pts[0].Step != 3 || pts[0].Value != 0.25 {
		t.Fatalf("incremental: %+v", pts)
	}
	if len(s.ScanJSONL(p)) != 0 {
		t.Fatal("expected no new points")
	}
}

func record(data []byte) []byte {
	var b []byte
	b = binary.LittleEndian.AppendUint64(b, uint64(len(data)))
	b = binary.LittleEndian.AppendUint32(b, 0) // crc (ignored)
	b = append(b, data...)
	b = binary.LittleEndian.AppendUint32(b, 0)
	return b
}

func event(step int64, value []byte) []byte {
	var sum []byte
	sum = protowire.AppendTag(sum, 1, protowire.BytesType)
	sum = protowire.AppendBytes(sum, value)
	var ev []byte
	ev = protowire.AppendTag(ev, 1, protowire.Fixed64Type)
	ev = protowire.AppendFixed64(ev, math.Float64bits(1.7e9))
	ev = protowire.AppendTag(ev, 2, protowire.VarintType)
	ev = protowire.AppendVarint(ev, uint64(step))
	ev = protowire.AppendTag(ev, 5, protowire.BytesType)
	ev = protowire.AppendBytes(ev, sum)
	return ev
}

func simpleValue(tag string, v float32) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, tag)
	b = protowire.AppendTag(b, 2, protowire.Fixed32Type)
	b = protowire.AppendFixed32(b, math.Float32bits(v))
	return b
}

func tensorValue(tag string, v float32) []byte {
	var tp []byte
	tp = protowire.AppendTag(tp, 1, protowire.VarintType)
	tp = protowire.AppendVarint(tp, 1) // DT_FLOAT
	tp = protowire.AppendTag(tp, 5, protowire.BytesType)
	tp = protowire.AppendBytes(tp, binary.LittleEndian.AppendUint32(nil, math.Float32bits(v)))
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, tag)
	b = protowire.AppendTag(b, 8, protowire.BytesType)
	b = protowire.AppendBytes(b, tp)
	return b
}

func TestTensorboard(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "val"), 0o755)
	var data []byte
	data = append(data, record(event(10, simpleValue("loss", 0.5)))...)
	data = append(data, record(event(20, tensorValue("reward", 2.5)))...)
	partial := record(event(30, simpleValue("loss", 0.1)))
	data = append(data, partial[:10]...)
	f := filepath.Join(dir, "events.out.tfevents.123.host")
	os.WriteFile(f, data, 0o644)
	os.WriteFile(filepath.Join(dir, "val", "events.out.tfevents.1.h"), record(event(5, simpleValue("loss", 0.7))), 0o644)
	s := NewScanner()
	pts := s.ScanTensorboard(dir)
	if len(pts) != 3 {
		t.Fatalf("got %+v", pts)
	}
	byKey := map[string]float64{}
	for _, p := range pts {
		byKey[p.Key] = p.Value
	}
	if byKey["loss"] != 0.5 || byKey["reward"] != 2.5 || math.Abs(byKey["val/loss"]-0.7) > 1e-6 {
		t.Fatalf("values: %+v", byKey)
	}
	fh, _ := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0)
	fh.Write(partial[10:])
	fh.Close()
	pts = s.ScanTensorboard(dir)
	if len(pts) != 1 || pts[0].Step != 30 {
		t.Fatalf("incremental: %+v", pts)
	}
}
