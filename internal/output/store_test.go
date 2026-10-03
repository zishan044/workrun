package output

import (
	"bytes"
	"strings"
	"sync"
	"testing"
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
