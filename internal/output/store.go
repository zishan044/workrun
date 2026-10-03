// Package output stores a bounded, sanitized display window for task output.
// Records from stdout and stderr are ordered by the first observed text in each
// fragment; the two streams have no reliable global write order.
package output

import (
	"io"
	"runtime"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const (
	DefaultMaxBytes       = 1 << 20
	DefaultMaxRecords     = 2000
	DefaultMaxRecordBytes = 8 << 10
	maxRawChunkBytes      = 16 << 10
	ansiDataLimit         = 1 << 10
	ansiParamsLimit       = 16
	tabSpaces             = 4
)

// Stream identifies the source of a display record.
type Stream string

const (
	Stdout Stream = "stdout"
	Stderr Stream = "stderr"
)

// Limits bounds retained display data and one logical line fragment. Values
// below four are raised to four so a complete UTF-8 rune can be represented.
type Limits struct {
	MaxBytes       int
	MaxRecords     int
	MaxRecordBytes int
}

// Record is one sanitized display fragment. Partial marks a currently open
// line; Continued marks a fragment that continues a previous overlong or
// evicted fragment from the same stream.
type Record struct {
	ID        uint64
	Stream    Stream
	Text      string
	Partial   bool
	Continued bool
}

// Snapshot is an immutable copy of the retained display window.
type Snapshot struct {
	Revision       uint64
	Records        []Record
	RetainedBytes  int
	EvictedBytes   uint64
	EvictedRecords uint64
}

// Store keeps independent streaming parser state for stdout and stderr while
// sharing a synchronized bounded record ring.
type Store struct {
	mu sync.Mutex

	limits         Limits
	records        []Record
	head           int
	count          int
	retainedBytes  int
	evictedBytes   uint64
	evictedRecords uint64
	revision       uint64
	nextID         uint64
	finished       bool
	stdout         *streamState
	stderr         *streamState
}

type streamState struct {
	mu        sync.Mutex
	store     *Store
	stream    Stream
	parser    *ansi.Parser
	pending   []byte
	line      strings.Builder
	open      *Record
	pendingCR bool
	continued bool
}

type streamWriter struct {
	store  *Store
	state  *streamState
}

// DefaultLimits returns the standard v0.1 display bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxBytes:       DefaultMaxBytes,
		MaxRecords:     DefaultMaxRecords,
		MaxRecordBytes: DefaultMaxRecordBytes,
	}
}

// NewStore creates a store using the requested limits, filling zero values with
// the standard limits and ensuring that a UTF-8 rune always fits in a record.
func NewStore(limits Limits) *Store {
	defaults := DefaultLimits()
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = defaults.MaxBytes
	}
	if limits.MaxRecords <= 0 {
		limits.MaxRecords = defaults.MaxRecords
	}
	if limits.MaxRecordBytes <= 0 {
		limits.MaxRecordBytes = defaults.MaxRecordBytes
	}
	if limits.MaxBytes < utf8.UTFMax {
		limits.MaxBytes = utf8.UTFMax
	}
	if limits.MaxRecordBytes < utf8.UTFMax {
		limits.MaxRecordBytes = utf8.UTFMax
	}
	if limits.MaxRecordBytes > limits.MaxBytes {
		limits.MaxRecordBytes = limits.MaxBytes
	}

	s := &Store{
		limits:  limits,
		records: make([]Record, limits.MaxRecords),
	}
	s.stdout = newStreamState(s, Stdout)
	s.stderr = newStreamState(s, Stderr)
	return s
}

func newStreamState(store *Store, stream Stream) *streamState {
	state := &streamState{
		store:   store,
		stream:  stream,
		parser:  ansi.NewParser(),
		pending: make([]byte, 0, utf8.UTFMax),
	}
	state.parser.SetParamsSize(ansiParamsLimit)
	state.parser.SetDataSize(ansiDataLimit)
	state.parser.SetHandler(ansi.Handler{
		Print:   state.printRuneLocked,
		Execute: state.executeControlLocked,
	})
	return state
}

// StdoutWriter returns the synchronized sanitizer for child stdout.
func (s *Store) StdoutWriter() io.Writer { return streamWriter{store: s, state: s.stdout} }

// StderrWriter returns the synchronized sanitizer for child stderr.
func (s *Store) StderrWriter() io.Writer { return streamWriter{store: s, state: s.stderr} }

func (w streamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	for offset := 0; offset < len(p); {
		end := offset + maxRawChunkBytes
		if end > len(p) {
			end = len(p)
		}
		w.store.mu.Lock()
		if !w.store.finished {
			for _, b := range p[offset:end] {
				w.state.feedByteLocked(b)
			}
			w.state.syncOpenLocked()
		}
		w.store.mu.Unlock()
		offset = end
		if offset < len(p) {
			runtime.Gosched()
		}
	}
	return len(p), nil
}

// SnapshotIfChanged returns a detached snapshot only when revision differs.
func (s *Store) SnapshotIfChanged(revision uint64) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision == s.revision {
		return Snapshot{}, false
	}
	records := make([]Record, s.count)
	for i := range s.count {
		records[i] = s.records[(s.head+i)%len(s.records)]
	}
	return Snapshot{
		Revision:       s.revision,
		Records:        records,
		RetainedBytes:  s.retainedBytes,
		EvictedBytes:   s.evictedBytes,
		EvictedRecords: s.evictedRecords,
	}, true
}

// Finish flushes incomplete UTF-8 as replacement characters, resolves a final
// carriage return, discards incomplete ANSI sequences, and closes partial lines.
// Call it only after writers have stopped.
func (s *Store) Finish() {
	s.stdout.mu.Lock()
	s.stderr.mu.Lock()
	defer s.stderr.mu.Unlock()
	defer s.stdout.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	for _, state := range []*streamState{s.stdout, s.stderr} {
		for len(state.pending) > 0 {
			state.pending = state.pending[1:]
			state.writeParserBytesLocked(replacementBytes)
		}
		if state.pendingCR {
			state.pendingCR = false
			state.finishLineLocked()
		}
		// An incomplete CSI/OSC/DCS/etc. has no display text of its own.
		state.parser.Reset()
		state.closePartialLocked()
	}
}

var replacementBytes = []byte(string(utf8.RuneError))

func (s *streamState) feedByteLocked(b byte) {
	if len(s.pending) == 0 {
		if b < utf8.RuneSelf {
			s.parser.Advance(b)
			return
		}
		switch {
		case b >= 0xc2 && b <= 0xdf, b >= 0xe0 && b <= 0xef, b >= 0xf0 && b <= 0xf4:
			s.pending = append(s.pending, b)
			return
		default:
			s.writeParserBytesLocked(replacementBytes)
			return
		}
	}

	s.pending = append(s.pending, b)
	for len(s.pending) > 0 && utf8.FullRune(s.pending) {
		r, size := utf8.DecodeRune(s.pending)
		if r == utf8.RuneError && size == 1 {
			s.writeParserBytesLocked(replacementBytes)
			s.pending = s.pending[1:]
			continue
		}
		var encoded [utf8.UTFMax]byte
		n := utf8.EncodeRune(encoded[:], r)
		s.writeParserBytesLocked(encoded[:n])
		s.pending = s.pending[size:]
	}
}


func (s *streamState) writeParserBytesLocked(data []byte) {
	for _, b := range data {
		s.parser.Advance(b)
	}
}

func (s *streamState) printRuneLocked(r rune) {
	if s.pendingCR {
		s.pendingCR = false
		s.finishLineLocked()
	}
	if unicode.IsPrint(r) {
		s.appendRuneLocked(r)
	}
}

func (s *streamState) executeControlLocked(b byte) {
	switch b {
	case '\n':
		s.pendingCR = false
		s.finishLineLocked()
	case '\r':
		if s.pendingCR {
			s.pendingCR = false
			s.finishLineLocked()
		}
		s.pendingCR = true
	case '\t':
		if s.pendingCR {
			s.pendingCR = false
			s.finishLineLocked()
		}
		for range tabSpaces {
			s.appendRuneLocked(' ')
		}
	default:
		if s.pendingCR {
			s.pendingCR = false
			s.finishLineLocked()
		}
	}
}

func (s *streamState) appendRuneLocked(r rune) {
	text := string(r)
	textBytes := len(text)
	if s.open != nil && s.line.Len()+textBytes > s.store.limits.MaxRecordBytes {
		s.finishFragmentLocked()
	}
	for s.store.retainedBytes+textBytes > s.store.limits.MaxBytes && s.store.count > 0 {
		s.store.evictOldestLocked()
	}
	if s.open == nil {
		s.openRecordLocked()
	}
	s.line.WriteString(text)
	s.store.retainedBytes += textBytes
	s.store.revision++
}

func (s *streamState) openRecordLocked() {
	store := s.store
	store.nextID++
	if store.nextID == 0 {
		store.nextID++
	}
	if store.count == len(store.records) {
		store.evictOldestLocked()
	}
	index := (store.head + store.count) % len(store.records)
	store.records[index] = Record{
		ID:        store.nextID,
		Stream:    s.stream,
		Partial:   true,
		Continued: s.continued,
	}
	s.continued = false
	s.open = &store.records[index]
	s.line.Reset()
	store.count++
	store.revision++
}

func (s *streamState) syncOpenLocked() {
	if s.open == nil {
		return
	}
	s.open.Text = s.line.String()
	s.open.Partial = true
}

func (s *streamState) finishLineLocked() {
	if s.open == nil {
		s.openRecordLocked()
	}
	s.syncOpenLocked()
	s.open.Partial = false
	s.store.revision++
	s.open = nil
	s.line.Reset()
	s.continued = false
}

func (s *streamState) finishFragmentLocked() {
	if s.open == nil {
		return
	}
	s.syncOpenLocked()
	s.open.Partial = false
	s.store.revision++
	s.open = nil
	s.line.Reset()
	s.continued = true
}

func (s *streamState) closePartialLocked() {
	if s.open == nil {
		return
	}
	s.syncOpenLocked()
	s.open.Partial = false
	s.store.revision++
	s.open = nil
	s.line.Reset()
}

func (s *Store) evictOldestLocked() {
	if s.count == 0 {
		return
	}
	index := s.head
	record := &s.records[index]
	bytes := len(record.Text)
	for _, state := range []*streamState{s.stdout, s.stderr} {
		if state.open == record {
			bytes = state.line.Len()
			state.open = nil
			state.line.Reset()
			state.continued = true
		}
	}
	s.retainedBytes -= bytes
	s.evictedBytes += uint64(bytes)
	s.evictedRecords++
	*record = Record{}
	s.head = (s.head + 1) % len(s.records)
	s.count--
	s.revision++
}
