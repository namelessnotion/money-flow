// Package tlatrace records what the command handlers actually did, as traces
// TLC checks are behaviors of spec/eventstore.tla (spec/Traceeventstore.tla
// does the checking). The spec is only a design until something ties it to
// the code: a trace it rejects means the handlers took a step the design
// doesn't allow.
//
// A trace line is one step as it was observed: a Request received, a stream
// loaded, an append landing or losing its race, an answer returned. The
// Store decorator writes its lines after the database has answered, so the
// order of lines across Requests is not the order things happened in the
// database. Each Request's own lines are in order, and what they observed
// (stream lengths, expected sequences, outcomes) is what TLC uses to find
// the interleaving that really happened.
package tlatrace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// The kinds of trace line, one per step of spec/eventstore.tla they
// constrain.
const (
	KindReceive      = "Receive"
	KindLoad         = "Load"
	KindAppend       = "Append"
	KindAppendAtomic = "AppendAtomic"
	KindAnswer       = "Answer"
)

// Outcomes of an append, and the answers that aren't a decision.
const (
	OutcomeOK        = "ok"
	OutcomeConflict  = "conflict"
	OutcomeError     = "error"
	OutcomeAborted   = "Aborted"   // twirp.Aborted: lost every race
	OutcomeFailed    = "Failed"    // any other error: may or may not have decided
	OutcomeUndecided = "Undecided" // a response with no decision in it
)

// Record is one trace line. Which fields are set depends on Kind; Req is
// always written, and is empty for a store call made on nobody's behalf.
type Record struct {
	Kind        string   `json:"kind"`
	Req         string   `json:"req"`
	Command     string   `json:"command,omitempty"`
	Stream      string   `json:"stream,omitempty"`
	Streams     []string `json:"streams,omitempty"`
	Len         *int     `json:"len,omitempty"`
	ExpectedSeq *int64   `json:"expectedSeq,omitempty"`
	Types       []string `json:"types,omitempty"`
	Outcome     string   `json:"outcome,omitempty"`
}

func (r Record) touches(stream string) bool {
	if r.Stream == stream {
		return true
	}
	for _, s := range r.Streams {
		if s == stream {
			return true
		}
	}
	return false
}

// Header is a trace's first line: the constants it is checked under, taken
// from the Go code so the spec never holds its own copy of them.
type Header struct {
	MaxAttempts int                 `json:"maxAttempts"`
	Handlers    map[string][]string `json:"handlers"` // command -> event types it can be decided as
}

// Recorder collects Records from every goroutine handling a traced Request.
type Recorder struct {
	mu       sync.Mutex
	records  []Record
	requests atomic.Int64
}

func NewRecorder() *Recorder {
	return &Recorder{}
}

func (r *Recorder) record(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
}

func (r *Recorder) nextRequest() string {
	return fmt.Sprintf("r%d", r.requests.Add(1))
}

// Records returns every line recorded so far, in the order recorded.
func (r *Recorder) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.records...)
}

// WriteStreams writes one NDJSON trace per command stream into dir, named
// <stream>.ndjson: header first, then every line the stream's Requests
// recorded on that stream. Lines on other streams are dropped; they are the
// inputs to a decision, which the spec leaves open. It returns the paths
// written.
func (r *Recorder) WriteStreams(dir string, header Header) ([]string, error) {
	records := r.Records()

	streamOf := map[string]string{} // Request -> the stream it commanded
	var streams []string
	seen := map[string]bool{}
	for _, rec := range records {
		if rec.Kind != KindReceive {
			continue
		}
		streamOf[rec.Req] = rec.Stream
		if !seen[rec.Stream] {
			seen[rec.Stream] = true
			streams = append(streams, rec.Stream)
		}
	}

	headerLine := struct {
		Kind string `json:"kind"`
		Header
	}{Kind: "header", Header: header}

	paths := make([]string, 0, len(streams))
	for _, stream := range streams {
		lines := []any{headerLine}
		for _, rec := range records {
			if s, traced := streamOf[rec.Req]; traced && s == stream && rec.touches(stream) {
				lines = append(lines, rec)
			}
		}
		path := filepath.Join(dir, stream+".ndjson")
		if err := writeNDJSON(path, lines); err != nil {
			return paths, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func writeNDJSON(path string, lines []any) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("tlatrace: create %s: %w", path, err)
	}
	enc := json.NewEncoder(f)
	for _, line := range lines {
		if err := enc.Encode(line); err != nil {
			_ = f.Close()
			return fmt.Errorf("tlatrace: write %s: %w", path, err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("tlatrace: close %s: %w", path, err)
	}
	return nil
}
