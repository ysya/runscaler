// Package diagnostics stores bounded private failure reports, never health output.
package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/ysya/runscaler/internal/provider"
)

const maxExcerpt = 64 * 1024

type Record struct {
	Timestamp time.Time `json:"timestamp"`
	ScaleSet  string    `json:"scaleset"`
	Provider  string    `json:"provider"`
	Runner    string    `json:"runner"`
	Instance  string    `json:"instance"`
	Reason    string    `json:"reason"`
	provider.ExitDiagnostics
	Excerpt string `json:"excerpt,omitempty"`
}
type Store struct {
	dir     string
	capture bool
	mu      sync.Mutex
}

func NewStore(dir string, captureLogs bool) *Store { return &Store{dir: dir, capture: captureLogs} }

var authorization = regexp.MustCompile(`(?im)authorization\s*[:=][^\r\n]*`)
var pem = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?(-----END [A-Z0-9 ]+-----|$)`)

func excerpt(raw string, secrets []string) string {
	// Bound input too. Redact before truncating output to avoid token fragments.
	for _, secret := range secrets {
		if secret != "" {
			raw = strings.ReplaceAll(raw, secret, "[REDACTED]")
			// Collection can end at its byte limit in the middle of a known
			// credential. Remove a trailing prefix too, before output limits.
			for n := min(len(secret)-1, len(raw)); n >= 4; n-- {
				if strings.HasSuffix(raw, secret[:n]) {
					raw = raw[:len(raw)-n] + "[REDACTED]"
					break
				}
			}
		}
	}
	raw = authorization.ReplaceAllString(raw, "Authorization: [REDACTED]")
	raw = pem.ReplaceAllString(raw, "[REDACTED PEM]")
	lines := strings.Split(raw, "\n")
	if len(lines) > 100 {
		lines = lines[len(lines)-100:]
	}
	raw = strings.Join(lines, "\n")
	if len(raw) > maxExcerpt {
		raw = raw[:maxExcerpt]
	}
	return raw
}

func (s *Store) Save(ctx context.Context, r Record, secrets ...string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.Excerpt = ""
	if s.capture {
		r.Excerpt = excerpt(r.RawLog, secrets)
	}
	r.RawLog = ""
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	dir, err := privateDir(s.dir)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	fd := int(dir.Fd())
	name := "runner-failure-" + uuid.NewString() + ".json"
	temp := "." + name + ".tmp"
	f, err := unix.Openat(fd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(f), temp)
	defer func() { _ = unix.Unlinkat(fd, temp, 0) }()
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = unix.Renameat(fd, temp, fd, name); err != nil {
		return "", err
	}
	if err = prune(dir, time.Now()); err != nil {
		return filepath.Join(s.dir, name), err
	}
	return filepath.Join(s.dir, name), nil
}

// Walk with directory descriptors so replacement of a path cannot redirect a
// privileged writer. Existing private directories must already be safe.
func privateDir(path string) (*os.File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	for _, part := range parts {
		if e := unix.Mkdirat(fd, part, 0700); e != nil && e != unix.EEXIST {
			_ = unix.Close(fd)
			return nil, e
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if st.Mode&0777 != 0700 || st.Uid != uint32(os.Geteuid()) {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("diagnostics directory must be owned by runtime user with mode 0700")
	}
	return os.NewFile(uintptr(fd), abs), nil
}

func ownedReport(name string) bool {
	if !strings.HasPrefix(name, "runner-failure-") || !strings.HasSuffix(name, ".json") {
		return false
	}
	_, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(name, "runner-failure-"), ".json"))
	return err == nil
}
func prune(dir *os.File, now time.Time) error {
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	type report struct {
		name     string
		size     int64
		modified time.Time
	}
	var reports []report
	var total int64
	for _, entry := range entries {
		if !ownedReport(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			continue
		}
		reports = append(reports, report{entry.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].modified.Before(reports[j].modified) })
	count := len(reports)
	for _, r := range reports {
		if count <= 20 && total <= 2*1024*1024 && now.Sub(r.modified) <= 7*24*time.Hour {
			break
		}
		if err := unix.Unlinkat(int(dir.Fd()), r.name, 0); err != nil && err != unix.ENOENT {
			return err
		}
		count--
		total -= r.size
	}
	return nil
}
