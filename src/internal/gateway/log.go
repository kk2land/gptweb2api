package gateway

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// RotatingLog also serves as the SDK writer. Redact complete records, including
// records split across multiple Write calls, before they reach disk.
type RotatingLog struct {
	mu      sync.Mutex
	path    string
	max     int64
	backups int
	file    *os.File
	size    int64
	pending []byte
	secrets []string
}

func OpenLog(path string, maxMB, backups int, secrets ...string) (*RotatingLog, error) {
	l := &RotatingLog{path: path, max: int64(maxMB) * 1024 * 1024, backups: backups, secrets: secrets}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *RotatingLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.file, l.size = f, st.Size()
	return nil
}

func (l *RotatingLog) record(b []byte) error {
	s := string(b)
	for _, secret := range l.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	if len(s) > 32768 {
		s = "[oversized log record omitted]"
	}
	s += "\n"
	if l.size+int64(len(s)) > l.max {
		if err := l.file.Close(); err != nil {
			return err
		}
		for i := l.backups; i >= 1; i-- {
			old := l.path
			if i > 1 {
				old = fmt.Sprintf("%s.%d", l.path, i-1)
			}
			err := os.Rename(old, fmt.Sprintf("%s.%d", l.path, i))
			if err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := l.open(); err != nil {
			return err
		}
	}
	n, err := io.WriteString(l.file, s)
	l.size += int64(n)
	return err
}

func (l *RotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	l.pending = append(l.pending, p...)
	for {
		i := bytes.IndexByte(l.pending, '\n')
		if i < 0 {
			break
		}
		if err := l.record(l.pending[:i]); err != nil {
			return 0, err
		}
		l.pending = l.pending[i+1:]
	}
	if len(l.pending) > 65536 {
		l.pending = []byte("[oversized log record omitted]")
	}
	return n, nil
}

func (l *RotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) > 0 {
		if err := l.record(l.pending); err != nil {
			return err
		}
		l.pending = nil
	}
	return l.file.Close()
}
