package commands

import (
	"encoding/json"
	"testing"
)

// The --json failure document is the only thing an agent has to go on when a
// command fails, so it has to survive whatever the server put in the message.
func TestErrorJSONStaysParseable(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"plain", "node gpu-a is offline"},
		{"quotes", `recipe "train.yaml" is invalid`},
		{"ansi from a coloured traceback", "Traceback:\x1b[31mRuntimeError\x1b[0m: CUDA OOM"},
		{"carriage returns from tqdm", "epoch 1\r epoch 2\r"},
		{"invalid utf-8", "bad byte: \xff\xfe"},
		{"newlines", "line one\nline two"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got struct {
				Error    string `json:"error"`
				Code     string `json:"code"`
				ExitCode int    `json:"exit_code"`
			}
			if err := json.Unmarshal(errorJSON(c.msg, "backend_error", 4), &got); err != nil {
				t.Fatalf("output is not valid JSON: %v", err)
			}
			if got.Code != "backend_error" || got.ExitCode != 4 {
				t.Errorf("code/exit = %q/%d, want backend_error/4", got.Code, got.ExitCode)
			}
			if got.Error == "" {
				t.Error("error message was dropped")
			}
		})
	}
}
