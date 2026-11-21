package engine

// Owner: workstream A (supervisor stdio, attach, logs, wait, stats).

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// logsMaxFileBytes is the soft size cap for a container's persisted log file
// and in-memory log ring (~16 MiB); when exceeded, the oldest half is dropped.
const logsMaxFileBytes = 16 << 20

// logsEntry is one json-file log record in logs/<id>.log.
type logsEntry struct {
	Log    string    `json:"log"`
	Stream string    `json:"stream"` // "stdout" or "stderr"
	Time   time.Time `json:"time"`
}

func (l logsEntry) approxBytes() int64 {
	// JSON wrapper + timestamp + log string.
	return int64(len(l.Log) + 64)
}

// logsSplitEntries breaks raw output bytes into log entries split after each
// '\n', preserving exact bytes (including partial chunks without '\n').
func logsSplitEntries(stream string, data []byte, now time.Time) []logsEntry {
	if len(data) == 0 {
		return nil
	}
	var out []logsEntry
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			out = append(out, logsEntry{Log: string(data), Stream: stream, Time: now})
			break
		}
		out = append(out, logsEntry{Log: string(data[:i+1]), Stream: stream, Time: now})
		data = data[i+1:]
	}
	return out
}

func logsEncodeEntry(e logsEntry) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// logsReadFile reads docker json-file lines from path. A missing file returns
// nil, 0, nil. Malformed lines (such as a partial write on crash) are skipped.
func logsReadFile(path string) ([]logsEntry, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var (
		entries []logsEntry
		nbytes  int64
	)
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		nbytes += int64(len(line))
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var e logsEntry
			if jerr := json.Unmarshal(trimmed, &e); jerr == nil && e.Stream != "" {
				entries = append(entries, e)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return entries, nbytes, err
		}
	}
	return entries, nbytes, nil
}

// logsCompactFile drops the oldest half of lines from path atomically and
// returns the new file size.
func logsCompactFile(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	var lines [][]byte
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			lines = append(lines, line)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			_ = f.Close()
			return 0, rerr
		}
	}
	_ = f.Close()

	keep := lines[len(lines)/2:]
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, line := range keep {
		n, werr := tmp.Write(line)
		total += int64(n)
		if werr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return 0, werr
		}
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	return total, nil
}

// streamWriteMuxFrame writes one Docker multiplexed stream frame:
// [stream, 0, 0, 0, len BE32] followed by payload.
// stream is 1 for stdout, 2 for stderr.
func streamWriteMuxFrame(w io.Writer, stream byte, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var hdr [8]byte
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// logsParseTime parses a Docker API timestamp query parameter (since/until):
// empty or "0" yields zero time; otherwise RFC3339Nano, RFC3339, or Unix
// seconds (integer or fractional).
func logsParseTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	secStr, fracStr, hasFrac := strings.Cut(raw, ".")
	sec, err := strconv.ParseInt(secStr, 10, 64)
	if err != nil {
		return time.Time{}, errInvalid("invalid timestamp: %q", raw)
	}
	var nsec int64
	if hasFrac && fracStr != "" {
		if len(fracStr) > 9 {
			fracStr = fracStr[:9]
		}
		for len(fracStr) < 9 {
			fracStr += "0"
		}
		nsec, err = strconv.ParseInt(fracStr, 10, 64)
		if err != nil || nsec < 0 {
			return time.Time{}, errInvalid("invalid timestamp: %q", raw)
		}
	}
	return time.Unix(sec, nsec).UTC(), nil
}

// logsTailEntries returns the slice of entries covering the last tailN logical
// lines (counting '\n'-terminated lines plus a trailing partial line, if any).
// tailN < 0 returns all entries; tailN == 0 returns nil.
func logsTailEntries(entries []logsEntry, tailN int) []logsEntry {
	if tailN < 0 {
		return entries
	}
	if tailN == 0 || len(entries) == 0 {
		return nil
	}
	lines := 0
	if !strings.HasSuffix(entries[len(entries)-1].Log, "\n") {
		lines = 1
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if strings.HasSuffix(entries[i].Log, "\n") {
			if lines == tailN {
				return entries[i+1:]
			}
			lines++
		}
	}
	return entries
}
