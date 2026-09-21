package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Printer writes command output as either a plain table or JSON.
type Printer struct {
	JSON bool
	W    io.Writer
}

// PrintJSON writes v as indented JSON followed by a newline.
func (p Printer) PrintJSON(v any) error {
	enc := json.NewEncoder(p.W)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

// Table writes rows as a padded table. In JSON mode it writes an array of
// objects keyed by the lowercased headers instead.
func (p Printer) Table(headers []string, rows [][]string) error {
	if p.JSON {
		out := make([]map[string]string, 0, len(rows))
		for _, row := range rows {
			obj := make(map[string]string, len(headers))
			for i, h := range headers {
				if i < len(row) {
					obj[strings.ToLower(h)] = row[i]
				}
			}
			out = append(out, obj)
		}
		return p.PrintJSON(out)
	}

	tw := tabwriter.NewWriter(p.W, 0, 0, 2, ' ', 0)
	if len(headers) > 0 {
		_, _ = fmt.Fprintln(tw, strings.Join(headers, "\t"))
	}
	for _, row := range rows {
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

// printError writes err to w. In JSON mode it is emitted as a JSON object so
// callers can parse failures the same way they parse results.
func printError(w io.Writer, asJSON bool, code int, err error) {
	if asJSON {
		payload := map[string]any{
			"error": map[string]any{"code": code, "message": err.Error()},
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}
	_, _ = fmt.Fprintln(w, "syncthis:", err.Error())
}
