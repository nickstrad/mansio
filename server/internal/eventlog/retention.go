package eventlog

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// Retention: the log rolls files by hour and size, and something must delete them or a busy week fills the volume:
// everything that accumulates needs a reaper. The limits here are the only retention and are enforced whether
// or not anything reads the files: a log collector is expected to ship them first. A file deleted here before it
// was shipped is telemetry lost; it is counted (Stats.Retired) and logged.

// Retention defaults.
const (
	DefaultMaxTotalBytes = 4 << 30
	DefaultMaxAge        = 45 * 24 * time.Hour
	// retentionKeep protects files written recently: another process of the same role may hold one open, and
	// a collector may be reading the newest ones.
	retentionKeep = 10 * time.Minute
)

// eventFile matches the files the log writes (and their gzipped form): only these are ever deleted.
var eventFile = regexp.MustCompile(`^events-\d{2}-\d+-\d{3,}\.jsonl(\.gz)?$`)

// dateDir matches a role's per-day directories.
var dateDir = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// logFile is one event file of this log's role.
type logFile struct {
	path string
	size int64
	mod  time.Time
}

// planRetention picks the files to delete, oldest first: every file older than maxAge, then the oldest of the
// rest until what stays totals at most maxBytes. A file modified within keep of now, and open (this log's
// current file), are never picked: the total can exceed maxBytes by them. Pure.
//
//	pre: maxBytes <= 0 means no size limit, maxAge <= 0 no age limit
//	post: the result is in deletion order and holds no file newer than keep, nor open
func planRetention(files []logFile, now time.Time, maxBytes int64, maxAge, keep time.Duration, open string) []logFile {
	files = slices.Clone(files)
	slices.SortFunc(files, func(a, b logFile) int { return cmp.Or(a.mod.Compare(b.mod), cmp.Compare(a.path, b.path)) })
	var total int64
	for _, f := range files {
		total += f.size
	}
	var out []logFile
	for _, f := range files {
		old := maxAge > 0 && now.Sub(f.mod) > maxAge
		over := maxBytes > 0 && total > maxBytes
		if !old && !over {
			break // oldest first: no later file is old, and the total only shrinks
		}
		if f.path == open || now.Sub(f.mod) < keep {
			continue // protected: it still counts toward the total
		}
		out = append(out, f)
		total -= f.size
	}
	return out
}

// listFiles returns this role's event files: <dir>/<role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl[.gz].
// Anything else under the directory is left alone.
func (l *Log) listFiles() ([]logFile, error) {
	root := filepath.Join(l.cfg.Dir, l.cfg.Role)
	days, err := os.ReadDir(root)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot list the event log directory").With("dir", root)
	}
	var files []logFile
	for _, d := range days {
		if !d.IsDir() || !dateDir.MatchString(d.Name()) {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			return nil, errs.Wrap(module, err, "cannot list the event log directory").With("dir", filepath.Join(root, d.Name()))
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !eventFile.MatchString(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue // deleted meanwhile
			}
			files = append(files, logFile{filepath.Join(root, d.Name(), e.Name()), info.Size(), info.ModTime()})
		}
	}
	return files, nil
}

// retire applies the retention limits to this role's files. The writer calls it when it opens a file (at
// most once an hour or per MaxFileBytes written), so the tree is at most the limit plus one file and what was
// written in the last retentionKeep. A failure is reported like a write failure and tried again at the next
// file.
func (l *Log) retire() {
	if l.cfg.MaxTotalBytes < 0 && l.cfg.MaxAge < 0 {
		return
	}
	files, err := l.listFiles()
	if err != nil {
		l.warn(err, "event log retention failed; files are not being deleted")
		return
	}
	var total, bytes int64
	for _, f := range files {
		total += f.size
	}
	doomed := planRetention(files, l.cfg.Now(), l.cfg.MaxTotalBytes, l.cfg.MaxAge, retentionKeep, l.path)
	n := 0
	for _, f := range doomed {
		if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
			l.warn(errs.Wrap(module, err, "cannot delete an event log file").With("path", f.path), "event log retention failed; files are not being deleted")
			break
		}
		n++
		bytes += f.size
		os.Remove(filepath.Dir(f.path)) // its day's directory, if that was the last file (fails otherwise)
	}
	l.retired.Add(int64(n))
	l.retiredBytes.Add(bytes)
	l.treeBytes.Store(total - bytes)
	if n > 0 && l.cfg.Log != nil {
		l.cfg.Log.Info("event log retention deleted the oldest files", "event", "EVENTLOG.RETIRED", "files", n, "bytes", bytes,
			"left_bytes", total-bytes, "max_total_bytes", l.cfg.MaxTotalBytes, "max_age", l.cfg.MaxAge.String())
	}
}
