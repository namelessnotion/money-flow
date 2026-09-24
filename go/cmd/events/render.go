package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
)

const (
	reset = "\x1b[0m"
	bold  = "\x1b[1m"
	dim   = "\x1b[2m"
	red   = "\x1b[31m"
	green = "\x1b[32m"
)

// Each aggregate id gets one of these, so a stream can be followed by eye
// through the interleaved log.
var idColors = []string{"\x1b[33m", "\x1b[34m", "\x1b[35m", "\x1b[36m", "\x1b[91m", "\x1b[92m", "\x1b[93m", "\x1b[94m"}

var typeColors = map[string]string{
	"transaction": "\x1b[1;35m",
	"transfer":    "\x1b[1;36m",
	"token":       "\x1b[33m",
	"wallet":      "\x1b[32m",
	"holder":      "\x1b[37m",
}

// Event names are past-tense facts; these words mark the ones a reader
// watching a Transaction most wants to spot.
var (
	failureWords = []string{"Rejected", "Failed", "Cancelled", "Rollback", "RolledBack", "Returned"}
	successWords = []string{"Completed", "Committed", "Performed"}
)

// typeWidth lines the columns up: it is the longest aggregate type's length.
const typeWidth = len("transaction")

// renderer writes one event per line, either for a person or as JSON.
type renderer struct {
	json     bool
	color    bool
	payload  bool
	width    int // payload runes before truncating; 0 means no limit
	location *time.Location
}

func (r renderer) write(w io.Writer, e row) error {
	if r.json {
		return r.writeJSON(w, e)
	}
	return r.writeLine(w, e)
}

func (r renderer) writeLine(w io.Writer, e row) error {
	name := e.EventType[strings.LastIndex(e.EventType, ".")+1:]
	fields := []string{
		r.paint(dim, e.OccurredAt.In(r.location).Format("15:04:05.000")),
		r.paint(dim, fmt.Sprintf("#%d", e.GlobalSeq)),
		r.paint(typeColors[e.AggregateType], fmt.Sprintf("%-*s", typeWidth, e.AggregateType)),
		r.paint(idColor(e.AggregateID), shortID(e.AggregateID)),
		r.paint(dim, fmt.Sprintf("#%d", e.Sequence)),
		r.paint(outcomeColor(name)+bold, name),
	}
	if r.payload {
		fields = append(fields, r.paint(dim, truncate(r.payloadText(e), r.width)))
	}
	_, err := fmt.Fprintln(w, strings.Join(fields, "  "))
	return err
}

func (r renderer) writeJSON(w io.Writer, e row) error {
	payload, err := decodedPayload(e)
	if err != nil {
		payload, _ = json.Marshal(map[string]string{"undecodable": err.Error()})
	}
	line, err := json.Marshal(struct {
		GlobalSeq     int64           `json:"global_seq"`
		OccurredAt    time.Time       `json:"occurred_at"`
		AggregateType string          `json:"aggregate_type"`
		AggregateID   string          `json:"aggregate_id"`
		Sequence      int64           `json:"sequence"`
		EventType     string          `json:"event_type"`
		Payload       json.RawMessage `json:"payload"`
	}{e.GlobalSeq, e.OccurredAt, e.AggregateType, e.AggregateID, e.Sequence, e.EventType, payload})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", line)
	return err
}

func (r renderer) payloadText(e row) string {
	payload, err := decodedPayload(e)
	if err != nil {
		return "(undecodable: " + err.Error() + ")"
	}
	return string(payload)
}

// decodedPayload is the event as compact JSON. protojson deliberately varies
// its whitespace between runs, so it is compacted to one stable form.
func decodedPayload(e row) (json.RawMessage, error) {
	msg, err := e.Decode()
	if err != nil {
		return nil, err
	}
	raw, err := protojson.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (r renderer) paint(code, text string) string {
	if !r.color || code == "" {
		return text
	}
	return code + text + reset
}

// shortID keeps the tail of the id: ids are uuid v7, whose leading digits are
// a timestamp shared by everything written in the same moment.
func shortID(id string) string {
	const keep = 8
	if len(id) <= keep {
		return id
	}
	return "…" + id[len(id)-keep:]
}

func idColor(id string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return idColors[h.Sum32()%uint32(len(idColors))]
}

func outcomeColor(name string) string {
	for _, word := range failureWords {
		if strings.Contains(name, word) {
			return red
		}
	}
	for _, word := range successWords {
		if strings.Contains(name, word) {
			return green
		}
	}
	return ""
}

func truncate(s string, width int) string {
	runes := []rune(s)
	if width <= 0 || len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "…"
}
