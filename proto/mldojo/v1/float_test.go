package v1

import (
	"encoding/json"
	"math"
	"testing"
)

func TestMetricPointNaNRoundTrip(t *testing.T) {
	in := []MetricPoint{{Step: 1, Key: "loss", Value: math.NaN()}, {Step: 2, Key: "loss", Value: math.Inf(1)}, {Step: 3, Key: "loss", Value: 0.5}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out []MetricPoint
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(out[0].Value) || !math.IsInf(out[1].Value, 1) || out[2].Value != 0.5 {
		t.Fatalf("round trip: %s -> %+v", b, out)
	}
	m := map[string]Float{"x": Float(math.NaN())}
	if b, err := json.Marshal(m); err != nil || string(b) != `{"x":"NaN"}` {
		t.Fatalf("Float: %s %v", b, err)
	}
}
