package retention

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
)

type Policy struct {
	MaxBytes int64
	MaxAge   time.Duration
	Keep     int
}

type Writer struct {
	mu     sync.Mutex
	path   string
	policy Policy
	file   *os.File
}

func AvailableBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

func OpenWriter(path string, policy Policy) (*Writer, error) {
	if err := validate(policy); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w := &Writer{path: path, policy: policy}
	if err := w.rotateIfNeeded(0); err != nil {
		return nil, err
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rotateIfNeeded(int64(len(data))); err != nil {
		return 0, err
	}
	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	return w.file.Write(data)
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func RotateExisting(path string, policy Policy) error {
	if err := validate(policy); err != nil {
		return err
	}
	w := &Writer{path: path, policy: policy}
	return w.rotateIfNeeded(0)
}

func ArchiveAndReplace(path string, replacement []byte, policy Policy) error {
	if err := validate(policy); err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fsutil.WriteFile(path, replacement, 0o600)
	} else if err != nil {
		return err
	}
	if err := archive(path); err != nil {
		return err
	}
	if err := fsutil.WriteFile(path, replacement, 0o600); err != nil {
		return err
	}
	return pruneArchives(path, policy.Keep)
}

func WriteHistory(dst io.Writer, path string) error {
	archives, err := archives(path)
	if err != nil {
		return err
	}
	for _, archivePath := range archives {
		f, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		reader, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return err
		}
		_, copyErr := io.Copy(dst, reader)
		closeErr := reader.Close()
		_ = f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(dst, f)
	return err
}

func PruneRunDirs(root string, exclude map[string]bool, maxAge time.Duration, maxCount int, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type candidate struct {
		name string
		path string
		mod  time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		if !entry.IsDir() || exclude[entry.Name()] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate{entry.Name(), filepath.Join(root, entry.Name()), info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mod.After(candidates[j].mod) })
	var removed []string
	for index, item := range candidates {
		if index < maxCount && now.Sub(item.mod) <= maxAge {
			continue
		}
		if err := os.RemoveAll(item.path); err != nil {
			return removed, err
		}
		removed = append(removed, item.name)
	}
	return removed, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	return nil
}

func (w *Writer) rotateIfNeeded(incoming int64) error {
	info, err := os.Stat(w.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() == 0 || (info.Size()+incoming <= w.policy.MaxBytes && time.Since(info.ModTime()) <= w.policy.MaxAge) {
		return nil
	}
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	if err := archive(w.path); err != nil {
		return err
	}
	if err := fsutil.WriteFile(w.path, nil, 0o600); err != nil {
		return err
	}
	return pruneArchives(w.path, w.policy.Keep)
}

func archive(path string) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	archivePath := fmt.Sprintf("%s.%s.gz", path, time.Now().UTC().Format("20060102T150405.000000000Z"))
	temporary := archivePath + ".tmp"
	target, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(target)
	_, copyErr := io.Copy(gz, source)
	closeGzipErr := gz.Close()
	syncErr := target.Sync()
	closeErr := target.Close()
	if copyErr != nil || closeGzipErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		for _, candidate := range []error{copyErr, closeGzipErr, syncErr, closeErr} {
			if candidate != nil {
				return candidate
			}
		}
	}
	if err := os.Rename(temporary, archivePath); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func archives(path string) ([]string, error) {
	matches, err := filepath.Glob(path + ".*.gz")
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

func pruneArchives(path string, keep int) error {
	matches, err := archives(path)
	if err != nil {
		return err
	}
	for len(matches) > keep {
		if err := os.Remove(matches[0]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		matches = matches[1:]
	}
	return nil
}

func validate(policy Policy) error {
	if policy.MaxBytes < 1 || policy.MaxAge <= 0 || policy.Keep < 1 {
		return fmt.Errorf("invalid retention policy: max_bytes=%d max_age=%s keep=%d", policy.MaxBytes, policy.MaxAge, policy.Keep)
	}
	return nil
}
