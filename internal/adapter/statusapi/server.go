package statusapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func SocketPath(repository string) string {
	return filepath.Join(fmt.Sprintf("/tmp/codex-loop-status-%d", os.Geteuid()), fmt.Sprintf("%x.sock", sha256.Sum256([]byte(strings.ToLower(repository)))))
}

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func listen(path string) (*net.UnixListener, os.FileInfo, error) {
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !owned(info) {
		return nil, nil, errors.New("unsafe status socket directory")
	}
	if old, err := os.Lstat(path); err == nil {
		if old.Mode()&os.ModeSocket == 0 || !owned(old) {
			return nil, nil, errors.New("unsafe existing status socket")
		}
		conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, nil, errors.New("status socket already listening")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, nil, fmt.Errorf("probe status socket: %w", dialErr)
		}
		current, err := os.Lstat(path)
		if err != nil {
			return nil, nil, err
		}
		if !os.SameFile(old, current) {
			return nil, nil, errors.New("status socket replaced during probe")
		}
		if err := os.Remove(path); err != nil {
			return nil, nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	listener.SetUnlinkOnClose(false)
	info, err = os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = removeOwned(path, info)
		return nil, nil, err
	}
	return listener, info, nil
}

func removeOwned(path string, original os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSocket == 0 || !owned(current) || !os.SameFile(original, current) {
		return errors.New("status socket replaced; leaving replacement intact")
	}
	return os.Remove(path)
}

// Start requires the repository supervisor lock to remain held until cleanup
// returns. API failure is diagnostic only and must not cancel the runtime.
func Start(ctx context.Context, handler *Handler, logger *log.Logger) (func(), error) {
	path := SocketPath(handler.Repository)
	listener, info, err := listen(path)
	if err != nil {
		return nil, err
	}
	handler.logger = logger
	server := &http.Server{ErrorLog: logger, Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	var once sync.Once
	done := make(chan struct{})
	cleanup := func() {
		once.Do(func() {
			if err := server.Close(); err != nil {
				logger.Printf("status API close: %v", err)
			}
			if err := removeOwned(path, info); err != nil {
				logger.Printf("status API cleanup: %v", err)
			}
			close(done)
		})
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("status API serve: %v", err)
		}
		cleanup()
	}()
	go func() {
		select {
		case <-ctx.Done():
			cleanup()
		case <-done:
		}
	}()
	return cleanup, nil
}
