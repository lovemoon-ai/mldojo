// Package output renders CLI results as human tables or JSON (--json).
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

type Printer struct {
	JSON bool
	Out  io.Writer
}

func New(asJSON bool) *Printer { return &Printer{JSON: asJSON, Out: os.Stdout} }

// Emit prints v as JSON when --json, otherwise calls human().
func (p *Printer) Emit(v any, human func(w io.Writer)) error {
	if p.JSON {
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	}
	human(p.Out)
	return nil
}

// Table writes aligned rows.
func Table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// KV writes "key: value" lines.
func KV(w io.Writer, pairs ...[2]string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, p := range pairs {
		fmt.Fprintf(tw, "%s:\t%s\n", p[0], p[1])
	}
	tw.Flush()
}

func Short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func Ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02")
}

func Dur(start, end *time.Time) string {
	if start == nil {
		return "-"
	}
	e := time.Now()
	if end != nil {
		e = *end
	}
	return e.Sub(*start).Round(time.Second).String()
}

func Bytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(u), 0
	for m := n / u; m >= u; m /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func Or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
