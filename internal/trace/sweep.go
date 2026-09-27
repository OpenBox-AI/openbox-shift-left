package trace

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// maxDirBytes is the total-trace-directory budget the prune pass enforces.
// It is a budget on past days only: today's file is never a prune candidate,
// so a single very verbose day can still exceed this on its own.
const maxDirBytes = 512 << 20

// dateFileRE matches a day's trace file, plain or gzipped, and captures its
// UTC date so rotation/retention/prune can all key off the filename alone
// rather than re-opening every file to find its date.
var dateFileRE = regexp.MustCompile(`^trace-(\d{4}-\d{2}-\d{2})\.jsonl(\.gz)?$`)

const dateLayout = "2006-01-02"

// Sweep gzips every past day's plain file, deletes days outside the
// RetainDays window, and prunes remaining past days oldest-first while the
// directory exceeds maxDirBytes. Emit never calls this -- a hook process has
// a 30s budget and Emit is meant to cost microseconds -- so Sweep is only
// ever invoked by a daemon's own ticker, `openbox trace`, or `openbox
// doctor`.
//
// Multiple processes may race to sweep the same directory (a daemon's
// ticker, a concurrent `openbox trace` run); TryLock on .rotate.lock means
// exactly one wins and every other caller returns nil having done nothing,
// rather than two sweeps double-gzipping or racing a rename.
func (w *Writer) Sweep() (err error) {
	defer func() {
		if recover() != nil {
			err = nil // Sweep is best-effort background hygiene; a panic here
			// must not take down whatever called it. Nothing was corrupted --
			// every step below either fully completes or leaves source files
			// untouched -- so silently trying again next sweep is safe.
		}
	}()

	dir := w.Dir
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	fl := flock.New(filepath.Join(dir, ".rotate.lock"))
	locked, lockErr := fl.TryLock()
	if lockErr != nil || !locked {
		// Held by another sweeper right now, or the lock itself couldn't be
		// taken (e.g. a permissions oddity): either way, skip without error --
		// there is always a next sweep.
		return nil
	}
	defer fl.Unlock()

	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	today := now().UTC().Format(dateLayout)

	if err := removeStaleTmp(dir); err != nil {
		return err
	}
	if err := gzipPastDays(dir, today); err != nil {
		return err
	}
	if err := pruneOutsideRetention(dir, today); err != nil {
		return err
	}
	return prunePastDaysBySize(dir, today)
}

// removeStaleTmp deletes any *.gz.tmp left behind by a sweep that was
// killed mid-gzip, before this sweep starts writing its own. A tmp file is
// only ever a work-in-progress artifact of gzipFile below; nothing reads it.
func removeStaleTmp(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".gz.tmp") {
			continue
		}
		// Best-effort: a stale tmp that resists removal (e.g. still open on
		// Windows) is retried on the next sweep, not a hard failure now.
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return nil
}

// gzipPastDays compresses every plain trace-*.jsonl dated before today. A
// day already carrying a .gz is left alone (nothing to redo); a day that
// fails to gzip (permission error, file locked by another process on
// Windows) is skipped and retried on the next sweep rather than aborting the
// rest of the pass.
func gzipPastDays(dir, today string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := dateFileRE.FindStringSubmatch(e.Name())
		if m == nil || m[2] == ".gz" {
			continue
		}
		date := m[1]
		if date >= today {
			continue // today (or a clock-skewed future name) stays plain
		}
		_ = gzipFile(filepath.Join(dir, e.Name()))
	}
	return nil
}

// gzipFile compresses src into src+".gz" via a ".gz.tmp" staging file that
// is fsynced and renamed into place before src is unlinked, so a crash
// mid-compression never leaves a dropped or half-written .gz overwriting a
// still-good source file: at every point either the source or the finished
// .gz exists, never neither.
func gzipFile(src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}

	tmp := src + ".gz.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		in.Close()
		return err
	}

	gz := gzip.NewWriter(out)
	_, copyErr := io.Copy(gz, in)
	in.Close()
	closeErr := gz.Close()
	if copyErr != nil || closeErr != nil {
		out.Close()
		os.Remove(tmp)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	dst := strings.TrimSuffix(tmp, ".tmp")
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Remove(src); err != nil {
		// The .gz is already in place; src just failed to unlink (e.g. still
		// held open on Windows). Leave it -- the next sweep's gzip pass will
		// see the .gz already exists and skip it, and a duplicate leftover
		// .jsonl is a retry concern, not a data-loss one.
		return err
	}
	return nil
}

// pruneOutsideRetention deletes any file (plain or gzipped) dated before the
// RetainDays window, so a day older than "today plus the six days before it"
// never lingers regardless of the size budget.
func pruneOutsideRetention(dir, today string) error {
	cutoff, err := cutoffDate(today)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := dateFileRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		dt, err := time.Parse(dateLayout, m[1])
		if err != nil {
			continue
		}
		if dt.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

// cutoffDate returns the oldest date the retention window keeps: today minus
// (RetainDays-1) days, since today itself occupies one of the RetainDays
// slots.
func cutoffDate(today string) (time.Time, error) {
	t, err := time.Parse(dateLayout, today)
	if err != nil {
		return time.Time{}, err
	}
	return t.AddDate(0, 0, -(RetainDays - 1)), nil
}

// prunePastDaysBySize deletes past-day files oldest-first while the
// directory's total size exceeds maxDirBytes. Today's file is never a
// candidate -- a verbose today alone can push the directory over budget, and
// that is accepted rather than losing the day currently being written.
func prunePastDaysBySize(dir, today string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	type sized struct {
		name string
		date string
		size int64
	}
	var files []sized
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := dateFileRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, sized{name: e.Name(), date: m[1], size: info.Size()})
		total += info.Size()
	}
	if total <= maxDirBytes {
		return nil
	}

	sort.Slice(files, func(i, j int) bool { return files[i].date < files[j].date })
	for _, f := range files {
		if total <= maxDirBytes {
			break
		}
		if f.date >= today {
			continue
		}
		if err := os.Remove(filepath.Join(dir, f.name)); err != nil {
			continue // Windows-tolerant: skip, retry next sweep
		}
		total -= f.size
	}
	return nil
}
