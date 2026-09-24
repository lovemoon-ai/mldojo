package v1

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Float is a float64 whose JSON form survives NaN/±Inf (as the strings
// "NaN", "Infinity", "-Infinity"), which training metrics do produce.
type Float float64

func (f Float) MarshalJSON() ([]byte, error) { return json.Marshal(FloatJSON(float64(f))) }

func (f *Float) UnmarshalJSON(b []byte) error {
	v, err := parseFloatJSON(b)
	*f = Float(v)
	return err
}

// FloatJSON returns a JSON-encodable representation of v.
func FloatJSON(v float64) any {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	}
	return v
}

func parseFloatJSON(b []byte) (float64, error) {
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		return 0, err
	}
	switch v := x.(type) {
	case float64:
		return v, nil
	case string:
		switch v {
		case "NaN", "nan":
			return math.NaN(), nil
		case "Infinity", "inf", "+Infinity":
			return math.Inf(1), nil
		case "-Infinity", "-inf":
			return math.Inf(-1), nil
		}
		return strconv.ParseFloat(v, 64)
	case nil:
		return math.NaN(), nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("invalid number %s", b)
}

type metricPointJSON struct {
	Step  int64           `json:"step"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
	TS    time.Time       `json:"ts"`
}

func (p MetricPoint) MarshalJSON() ([]byte, error) {
	v, _ := json.Marshal(FloatJSON(p.Value))
	return json.Marshal(metricPointJSON{Step: p.Step, Key: p.Key, Value: v, TS: p.TS})
}

func (p *MetricPoint) UnmarshalJSON(b []byte) error {
	var m metricPointJSON
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	v, err := parseFloatJSON(m.Value)
	if err != nil {
		return err
	}
	*p = MetricPoint{Step: m.Step, Key: m.Key, Value: v, TS: m.TS}
	return nil
}
