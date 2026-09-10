package runtimemetadata

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"golang.org/x/sys/unix"
)

var ErrNotRunning = errors.New("runtime process is not running")
var repositoryPattern = regexp.MustCompile(`^[a-z0-9_.-]+/[a-z0-9_.-]+$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type StartTime struct {
	Seconds      int64 `json:"seconds"`
	Microseconds int64 `json:"microseconds"`
}

func (s *StartTime) UnmarshalJSON(data []byte) error {
	var value struct {
		Seconds      *int64 `json:"seconds"`
		Microseconds *int64 `json:"microseconds"`
	}
	if err := fsutil.DecodeStrictJSON(data, &value); err != nil {
		return err
	}
	if value.Seconds == nil || value.Microseconds == nil {
		return errors.New("incomplete runtime process start time")
	}
	s.Seconds, s.Microseconds = *value.Seconds, *value.Microseconds
	return nil
}

type Process struct {
	PID           int
	UID           uint32
	BootSessionID string
	StartedAt     StartTime
}

type Record struct {
	SchemaVersion    int       `json:"schema_version"`
	Repository       string    `json:"repository"`
	RepoID           string    `json:"repo_id"`
	RuntimeVersion   string    `json:"runtime_version"`
	PID              int       `json:"pid"`
	BootSessionID    string    `json:"boot_session_id"`
	ProcessStartedAt StartTime `json:"process_started_at"`
	WrittenAt        time.Time `json:"written_at"`
}

type Observation struct {
	Version    string    `json:"version"`
	ObservedAt time.Time `json:"observed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type Store struct {
	Root    string
	Inspect func(int) (Process, error)
}

func (s Store) inspect(pid int) (Process, error) {
	if s.Inspect != nil {
		return s.Inspect(pid)
	}
	return InspectProcess(pid)
}

func repositoryKey(repository string) (string, error) {
	repository = strings.ToLower(repository)
	if !repositoryPattern.MatchString(repository) || strings.Contains(repository, "..") {
		return "", errors.New("invalid runtime repository")
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(repository))), nil
}

func (r Record) valid(repository, name string, now time.Time) bool {
	return r.SchemaVersion == 1 && r.Repository == strings.ToLower(repository) && idPattern.MatchString(r.RepoID) && name == r.RepoID+".json" &&
		strings.TrimSpace(r.RuntimeVersion) != "" && r.RuntimeVersion != "unknown" && len(r.RuntimeVersion) <= 128 &&
		!strings.ContainsAny(r.RuntimeVersion, "\r\n\x00") && r.PID > 0 && r.BootSessionID != "" &&
		r.ProcessStartedAt.Seconds > 0 && r.ProcessStartedAt.Microseconds >= 0 && r.ProcessStartedAt.Microseconds < 1000000 &&
		!r.WrittenAt.IsZero() && !r.WrittenAt.After(now) && !r.WrittenAt.Before(time.Unix(r.ProcessStartedAt.Seconds, r.ProcessStartedAt.Microseconds*1000))
}

// Publish is called while the repository supervisor lock is held. The returned
// cleanup must run before that lock is released; it only removes this record.
func (s Store) Publish(repository, repoID, version string, now time.Time) (func() error, error) {
	key, err := repositoryKey(repository)
	if err != nil {
		return nil, err
	}
	process, err := s.inspect(os.Getpid())
	if err != nil {
		return nil, err
	}
	record := Record{1, strings.ToLower(repository), repoID, version, process.PID, process.BootSessionID, process.StartedAt, now.UTC()}
	if process.PID != os.Getpid() || process.UID != uint32(os.Geteuid()) || !record.valid(repository, repoID+".json", now) {
		return nil, errors.New("invalid runtime identity")
	}
	if !filepath.IsAbs(s.Root) {
		return nil, errors.New("runtime metadata root must be absolute")
	}
	for _, dir := range []string{s.Root, filepath.Join(s.Root, "runtime-metadata"), filepath.Join(s.Root, "runtime-metadata", key)} {
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		file, err := openPrivateDir(dir)
		if err != nil {
			return nil, err
		}
		file.Close()
	}
	dir := filepath.Join(s.Root, "runtime-metadata", key)
	path := filepath.Join(dir, repoID+".json")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("runtime metadata is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := fsutil.WriteJSON(path, record, 0600); err != nil {
		return nil, err
	}
	return func() error {
		directory, err := s.openRepository(repository)
		if err != nil {
			return err
		}
		defer directory.Close()
		data, err := readRecord(directory, repoID+".json")
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var current Record
		if err := fsutil.DecodeStrictJSON(data, &current); err != nil {
			return err
		}
		if current != record {
			return errors.New("runtime metadata identity changed before cleanup")
		}
		return unix.Unlinkat(int(directory.Fd()), repoID+".json", 0)
	}, nil
}

// Observe never reads registry, assignment or supervisor snapshot data. A nil
// observation means the local runtime could not be uniquely verified.
func (s Store) Observe(repository string, now time.Time, timeout time.Duration) *Observation {
	if timeout <= 0 {
		return nil
	}
	directory, err := s.openRepository(repository)
	if err != nil {
		return nil
	}
	defer directory.Close()
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return nil
	}
	records := map[string][]byte{}
	var found *Record
	var identity Process
	for _, name := range names {
		if strings.HasPrefix(name, ".agent-loop-") {
			continue
		}
		data, err := readRecord(directory, name)
		if err != nil {
			return nil
		}
		records[name] = data
		var record Record
		if fsutil.DecodeStrictJSON(data, &record) != nil || !record.valid(repository, name, now) {
			return nil
		}
		process, err := s.inspect(record.PID)
		if errors.Is(err, ErrNotRunning) {
			continue
		}
		if err != nil {
			return nil
		}
		if process.UID != uint32(os.Geteuid()) || process.PID != record.PID || process.BootSessionID != record.BootSessionID || process.StartedAt != record.ProcessStartedAt {
			continue
		}
		if found != nil {
			return nil
		}
		found, identity = &record, process
	}
	if found == nil {
		return nil
	}
	check, err := s.openRepository(repository)
	if err != nil {
		return nil
	}
	defer check.Close()
	names, err = check.Readdirnames(-1)
	if err != nil {
		return nil
	}
	count := 0
	for _, name := range names {
		if strings.HasPrefix(name, ".agent-loop-") {
			continue
		}
		count++
		data, err := readRecord(check, name)
		if err != nil || !bytes.Equal(data, records[name]) {
			return nil
		}
	}
	if count != len(records) {
		return nil
	}
	process, err := s.inspect(found.PID)
	if err != nil || process != identity {
		return nil
	}
	return &Observation{found.RuntimeVersion, now.UTC(), now.Add(timeout).UTC()}
}

func (s Store) openRepository(repository string) (*os.File, error) {
	key, err := repositoryKey(repository)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(s.Root) {
		return nil, errors.New("runtime metadata root must be absolute")
	}
	directory, err := openPrivateDir(s.Root)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"runtime-metadata", key} {
		fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		directory.Close()
		if err != nil {
			return nil, err
		}
		directory = os.NewFile(uintptr(fd), name)
		if err := privateFile(directory, true); err != nil {
			directory.Close()
			return nil, err
		}
	}
	return directory, nil
}

func openPrivateDir(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := privateFile(file, true); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func privateFile(file *os.File, directory bool) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return errors.New("runtime metadata must be private and owned by the current user")
	}
	return nil
}

func readRecord(directory *os.File, name string) ([]byte, error) {
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".json") {
		return nil, errors.New("invalid runtime metadata filename")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := privateFile(file, false); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, err
	}
	if len(data) > 4096 {
		return nil, errors.New("runtime metadata exceeds 4096 bytes")
	}
	return data, nil
}
