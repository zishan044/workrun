package output

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"
)

func snapshot(t *testing.T, store *Store) Snapshot {
	t.Helper()
	got, changed := store.SnapshotIfChanged(0)
	if !changed {
		t.Fatal("expected changed snapshot")
	}
	return got
}

func TestWritersSanitizeStreamsAndKeepPartialLinesVisible(t *testing.T) {
	store := NewStore(DefaultLimits())
	_, _ = store.StdoutWriter().Write([]byte("hello\t"))
	_, _ = store.StderrWriter().Write([]byte("warning\n"))
	got := snapshot(t, store)
	if len(got.Records) != 2 || got.Records[0].Text != "hello    " || !got.Records[0].Partial {
		t.Fatalf("unexpected stdout record: %#v", got.Records)
	}
	if got.Records[1].Stream != Stderr || got.Records[1].Text != "warning" || got.Records[1].Partial {
		t.Fatalf("unexpected stderr record: %#v", got.Records[1])
	}
}

func TestSmallWritesAndSplitUTF8AndANSIAreIncremental(t *testing.T) {
	store := NewStore(DefaultLimits())
	writer := store.StdoutWriter()
	for _, b := range []byte("A界\x1b[31mB\x1b]0;title\aC\n") {
		if n, err := writer.Write([]byte{b}); n != 1 || err != nil {
			t.Fatalf("Write() = %d, %v", n, err)
		}
	}
	if got := snapshot(t, store).Records[0].Text; got != "A界BC" {
		t.Fatalf("sanitized text = %q", got)
	}
}

func TestConcurrentStreamsAndLargeWrites(t *testing.T) {
	store := NewStore(Limits{MaxBytes: 1 << 20, MaxRecords: 200, MaxRecordBytes: 1024})
	var wg sync.WaitGroup
	for _, writer := range []interface{ Write([]byte) (int, error) }{store.StdoutWriter(), store.StderrWriter()} {
		writer := writer
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := bytes.Repeat([]byte("x"), 128<<10)
			if n, err := writer.Write(payload); n != len(payload) || err != nil {
				t.Errorf("large Write() = %d, %v", n, err)
			}
		}()
	}
	wg.Wait()
	got := snapshot(t, store)
	if len(got.Records) != 2 || got.RetainedBytes > 1<<20 {
		t.Fatalf("store exceeded bounds: records=%d bytes=%d", len(got.Records), got.RetainedBytes)
	}
}

func TestEvictsByBytesAndRecordCountAndBoundsLongLines(t *testing.T) {
	store := NewStore(Limits{MaxBytes: 12, MaxRecords: 2, MaxRecordBytes: 5})
	_, _ = store.StdoutWriter().Write([]byte(strings.Repeat("a", 20)))
	_, _ = store.StderrWriter().Write([]byte("b\nc\n"))
	got := snapshot(t, store)
	if got.RetainedBytes > 12 || len(got.Records) > 2 || got.EvictedRecords == 0 || got.EvictedBytes == 0 {
		t.Fatalf("unexpected bounded snapshot: %#v", got)
	}
	for _, record := range got.Records {
		if len(record.Text) > 5 {
			t.Fatalf("oversized record retained: %#v", record)
		}
	}
}

func TestOversizedSingleWriteReturnsAllBytesAndKeepsPartial(t *testing.T) {
	store := NewStore(Limits{MaxBytes: 16, MaxRecords: 4, MaxRecordBytes: 8})
	payload := []byte(strings.Repeat("z", 4096))
	if n, err := store.StdoutWriter().Write(payload); n != len(payload) || err != nil {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	got := snapshot(t, store)
	if got.RetainedBytes > 16 || got.EvictedBytes == 0 || len(got.Records) == 0 || !got.Records[len(got.Records)-1].Partial {
		t.Fatalf("unexpected post-flood snapshot: %#v", got)
	}
}

func TestCRLFAcrossWritesAndStandaloneCR(t *testing.T) {
	store := NewStore(DefaultLimits())
	writer := store.StdoutWriter()
	_, _ = writer.Write([]byte("one\r"))
	_, _ = writer.Write([]byte("\ntwo\rthree"))
	store.Finish()
	got := snapshot(t, store)
	want := []string{"one", "two", "three"}
	if len(got.Records) != len(want) {
		t.Fatalf("records = %#v", got.Records)
	}
	for i, text := range want {
		if got.Records[i].Text != text {
			t.Fatalf("record %d = %q, want %q", i, got.Records[i].Text, text)
		}
	}
}

func TestSnapshotsAreDetachedAndFinishReplacesIncompleteUTF8(t *testing.T) {
	store := NewStore(DefaultLimits())
	_, _ = store.StdoutWriter().Write([]byte{'a', 0xe2})
	before := snapshot(t, store)
	before.Records[0].Text = "mutated"
	store.Finish()
	after := snapshot(t, store)
	if after.Records[0].Text != "a�" || after.Records[0].Partial {
		t.Fatalf("Finish() snapshot = %#v", after.Records[0])
	}
	if _, changed := store.SnapshotIfChanged(after.Revision); changed {
		t.Fatal("unchanged revision returned a snapshot")
	}
}

func TestHugeUnterminatedEscapeHasBoundedParserData(t *testing.T) {
	store := NewStore(DefaultLimits())
	payload := append([]byte("visible\x1b]"), bytes.Repeat([]byte("x"), 256<<10)...)
	_, _ = store.StdoutWriter().Write(payload)
	if n := len(store.stdout.parser.Data()); n > ansiDataLimit {
		t.Fatalf("parser retained %d bytes", n)
	}
	store.Finish()
	if got := snapshot(t, store).Records[0].Text; got != "visible" {
		t.Fatalf("visible text = %q", got)
	}
}

func TestEvictingOpenRecordDropsItsPartialText(t *testing.T) {
	store := NewStore(Limits{MaxBytes: 4, MaxRecords: 1, MaxRecordBytes: 4})
	_, _ = store.StdoutWriter().Write([]byte("old"))
	_, _ = store.StderrWriter().Write([]byte("new"))
	if store.stdout.open != nil || store.stdout.line.Len() != 0 || !store.stdout.continued {
		t.Fatal("evicted open stdout text remains reachable")
	}
	got := snapshot(t, store)
	if len(got.Records) != 1 || got.Records[0].Text != "new" {
		t.Fatalf("snapshot = %#v", got.Records)
	}
}

func FuzzSanitizerBoundsAndRemovesTerminalControls(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("plain text\n"),
		[]byte("A界e\u0301"),
		{0xe2, 0x82, 0xac},
		[]byte("\x1b[31mred\x1b[0m"),
		[]byte("\x1b]0;title\a"),
		[]byte("\x1b]52;c;secret\x1b\\"),
		[]byte("\x1bPunfinished"),
		{0xff, '\n', 0x00},
	} {
		f.Add(seed, uint8(1))
	}

	f.Fuzz(func(t *testing.T, input []byte, chunk uint8) {
		// Keep one fuzz iteration small and deterministic while exercising
		// arbitrary chunk boundaries, including UTF-8 and escape boundaries.
		if len(input) > 32<<10 {
			input = input[:32<<10]
		}
		limits := DefaultLimits()
		store := NewStore(limits)
		writer := store.StdoutWriter()
		chunkSize := int(chunk) + 1
		for offset := 0; offset < len(input); {
			end := offset + chunkSize
			if end > len(input) {
				end = len(input)
			}
			if _, err := writer.Write(input[offset:end]); err != nil {
				t.Fatalf("Write() error: %v", err)
			}
			offset = end
		}
		if len(store.stdout.parser.Data()) > ansiDataLimit || len(store.stdout.line.String()) > limits.MaxRecordBytes || len(store.stdout.pending) >= utf8.UTFMax {
			t.Fatal("sanitizer retained parser or partial-line data beyond its bound")
		}
		store.Finish()

		got, _ := store.SnapshotIfChanged(0)
		if got.RetainedBytes > limits.MaxBytes || len(got.Records) > limits.MaxRecords {
			t.Fatalf("store exceeded bounds: bytes=%d records=%d", got.RetainedBytes, len(got.Records))
		}
		for _, record := range got.Records {
			if !utf8.ValidString(record.Text) || len(record.Text) > limits.MaxRecordBytes {
				t.Fatalf("invalid or oversized display record: %#v", record)
			}
			for _, r := range record.Text {
				if !unicode.IsPrint(r) {
					t.Fatalf("display record contains terminal control or nonprintable rune %U", r)
				}
			}
		}
	})
}
