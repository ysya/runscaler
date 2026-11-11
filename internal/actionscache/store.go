package actionscache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

var (
	ErrConflict     = errors.New("cache entry already exists")
	ErrNotFound     = errors.New("cache entry not found")
	ErrSizeMismatch = errors.New("cache size does not match uploaded blocks")
	ErrIncomplete   = errors.New("cache upload is incomplete")
	ErrStagingFull  = errors.New("cache staging budget exceeded")
)

const maxPartBytes = int64(1 << 30)

type Reservation struct{ ID string }

type Entry struct {
	ID           string
	DatabaseID   int64
	RepositoryID string
	Scope        string
	Key          string
	Version      string
	Size         int64
	Parts        []string
}

type Store struct {
	root         string
	db           *sql.DB
	mu           sync.Mutex
	pins         map[string]int
	closed       bool
	stagingLimit int64
	activeBytes  int64
}

func (s *Store) SetStagingLimit(bytes int64) {
	s.mu.Lock()
	s.stagingLimit = bytes
	s.mu.Unlock()
}

type StoreStats struct {
	Entries int64
	Bytes   int64
}

func (s *Store) Stats(ctx context.Context) (StoreStats, error) {
	var stats StoreStats
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(size_bytes),0) FROM entries WHERE state='complete'`).Scan(&stats.Entries, &stats.Bytes)
	return stats, err
}

// ReadStoreStats opens an existing database read-only for operator reporting.
// It never creates a cache directory or starts the receiver.
func ReadStoreStats(ctx context.Context, root string) (StoreStats, error) {
	path := filepath.Join(root, "cache.sqlite")
	if _, err := os.Stat(path); err != nil {
		return StoreStats{}, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return StoreStats{}, err
	}
	defer func() { _ = db.Close() }()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return StoreStats{}, err
	}
	if version != 1 {
		return StoreStats{}, fmt.Errorf("unsupported native cache schema version %d", version)
	}
	var stats StoreStats
	err = db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(size_bytes),0) FROM entries WHERE state='complete'`).Scan(&stats.Entries, &stats.Bytes)
	return stats, err
}

func OpenStore(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("cache data directory must be absolute")
	}
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("cache data directory must be private and not a symlink")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	for _, name := range []string{"staging", "objects"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("cache storage directory must be private and not a symlink")
		}
	}
	dbPath := filepath.Join(root, "cache.sqlite")
	if info, err := os.Lstat(dbPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("cache database must be a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		_ = db.Close()
		return nil, err
	}
	if version != 0 && version != 1 {
		_ = db.Close()
		return nil, fmt.Errorf("unsupported native cache schema version %d", version)
	}
	if version == 0 {
		tx, err := db.Begin()
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		for _, statement := range []string{
			`CREATE TABLE IF NOT EXISTS entries (
			 id INTEGER PRIMARY KEY, object_id TEXT NOT NULL UNIQUE,
			 repository_id TEXT NOT NULL, scope TEXT NOT NULL, cache_key TEXT NOT NULL, version TEXT NOT NULL,
			 state TEXT NOT NULL, created_at INTEGER NOT NULL, completed_at INTEGER NOT NULL DEFAULT 0, accessed_at INTEGER NOT NULL,
			 expires_at INTEGER NOT NULL, size_bytes INTEGER NOT NULL DEFAULT 0, manifest TEXT,
			 UNIQUE(repository_id, scope, cache_key, version))`,
			`CREATE TABLE IF NOT EXISTS parts (object_id TEXT NOT NULL, block_id TEXT NOT NULL, file_name TEXT NOT NULL, size_bytes INTEGER NOT NULL,
			 PRIMARY KEY(object_id, block_id))`,
			`CREATE INDEX IF NOT EXISTS entry_lookup ON entries(repository_id, scope, version, state, completed_at)`,
			`PRAGMA user_version=1`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				_ = db.Close()
				return nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, err
		}
	}
	if err := os.Chmod(dbPath, 0600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{root: root, db: db, pins: make(map[string]int)}, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

func (s *Store) Reserve(ctx context.Context, p Principal, key, version string) (Reservation, error) {
	scope := p.FirstWritableScope()
	if scope == "" {
		return Reservation{}, errors.New("write permission required")
	}
	if key == "" || version == "" || len(key) > 512 || len(version) > 256 {
		return Reservation{}, errors.New("invalid cache key or version")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.NewString()
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO entries(object_id, repository_id, scope, cache_key, version, state, created_at, accessed_at, expires_at)
	 VALUES(?,?,?,?,?,'reserved',?,?,?)`, id, p.RepositoryID, scope, key, version, now, now, now+7200)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Reservation{}, ErrConflict
		}
		return Reservation{}, err
	}
	if err := os.Mkdir(filepath.Join(s.root, "staging", id), 0700); err != nil {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM entries WHERE object_id=?`, id)
		return Reservation{}, err
	}
	return Reservation{ID: id}, nil
}

func (s *Store) PutPart(ctx context.Context, id, blockID string, body io.Reader) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	if blockID == "" || len(blockID) > 256 {
		return errors.New("invalid block ID")
	}
	s.mu.Lock()
	if err := s.requireReserved(ctx, id); err != nil {
		s.mu.Unlock()
		return err
	}
	allowed := maxPartBytes
	limited := s.stagingLimit > 0
	if limited {
		var staged int64
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(parts.size_bytes),0) FROM parts
		 JOIN entries ON entries.object_id=parts.object_id WHERE entries.state='reserved'`).Scan(&staged); err != nil {
			s.mu.Unlock()
			return err
		}
		available := s.stagingLimit - staged - s.activeBytes
		if available <= 0 {
			s.mu.Unlock()
			return ErrStagingFull
		}
		if available < allowed {
			allowed = available
		}
		s.activeBytes += allowed
	}
	s.mu.Unlock()
	if limited {
		defer func() { s.mu.Lock(); s.activeBytes -= allowed; s.mu.Unlock() }()
	}
	dir := filepath.Join(s.root, "staging", id)
	temp, err := os.CreateTemp(dir, ".upload-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	size, err := io.Copy(temp, io.LimitReader(body, allowed+1))
	if err != nil {
		temp.Close()
		return err
	}
	if size > allowed {
		temp.Close()
		return ErrStagingFull
	}
	if err := ctx.Err(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireReserved(ctx, id); err != nil {
		return err
	}
	var oldName string
	_ = s.db.QueryRowContext(ctx, `SELECT file_name FROM parts WHERE object_id=? AND block_id=?`, id, blockID).Scan(&oldName)
	fileName := uuid.NewString()
	path := filepath.Join(dir, fileName)
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO parts(object_id,block_id,file_name,size_bytes) VALUES(?,?,?,?)
	 ON CONFLICT(object_id,block_id) DO UPDATE SET file_name=excluded.file_name,size_bytes=excluded.size_bytes`, id, blockID, fileName, size)
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	if oldName != "" {
		_ = os.Remove(filepath.Join(dir, oldName))
	}
	return nil
}

func (s *Store) PutBlob(ctx context.Context, id string, body io.Reader) error {
	if err := s.PutPart(ctx, id, "__single__", body); err != nil {
		return err
	}
	return s.SetBlockList(ctx, id, []string{"__single__"})
}

func (s *Store) SetBlockList(ctx context.Context, id string, blockIDs []string) error {
	if len(blockIDs) == 0 || len(blockIDs) > 50000 {
		return ErrIncomplete
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireReserved(ctx, id); err != nil {
		return err
	}
	for _, blockID := range blockIDs {
		var size int64
		if err := s.db.QueryRowContext(ctx, `SELECT size_bytes FROM parts WHERE object_id=? AND block_id=?`, id, blockID).Scan(&size); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrIncomplete
			}
			return err
		}
	}
	manifest, _ := json.Marshal(blockIDs)
	_, err := s.db.ExecContext(ctx, `UPDATE entries SET manifest=? WHERE object_id=?`, string(manifest), id)
	return err
}

func (s *Store) requireReserved(ctx context.Context, id string) error {
	var state string
	var expiry int64
	if err := s.db.QueryRowContext(ctx, `SELECT state,expires_at FROM entries WHERE object_id=?`, id).Scan(&state, &expiry); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if state != "reserved" || expiry <= time.Now().Unix() {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Finalize(ctx context.Context, p Principal, key, version string, declaredSize int64) (Entry, error) {
	if declaredSize < 0 {
		return Entry{}, ErrSizeMismatch
	}
	scope := p.FirstWritableScope()
	if scope == "" {
		return Entry{}, errors.New("write permission required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var entry Entry
	var state string
	var expiry int64
	var manifest string
	err := s.db.QueryRowContext(ctx, `SELECT id,object_id,state,expires_at,manifest FROM entries
	 WHERE repository_id=? AND scope=? AND cache_key=? AND version=?`, p.RepositoryID, scope, key, version).
		Scan(&entry.DatabaseID, &entry.ID, &state, &expiry, &manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, err
	}
	if state != "reserved" || expiry <= time.Now().Unix() {
		return Entry{}, ErrNotFound
	}
	if manifest == "" {
		return Entry{}, ErrIncomplete
	}
	if err := json.Unmarshal([]byte(manifest), &entry.Parts); err != nil || len(entry.Parts) == 0 {
		return Entry{}, ErrIncomplete
	}
	var size int64
	for _, blockID := range entry.Parts {
		var partSize int64
		if err := s.db.QueryRowContext(ctx, `SELECT size_bytes FROM parts WHERE object_id=? AND block_id=?`, entry.ID, blockID).Scan(&partSize); err != nil {
			return Entry{}, ErrIncomplete
		}
		name := s.stagedPartPath(entry.ID, blockID)
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() != partSize {
			return Entry{}, ErrIncomplete
		}
		size += partSize
	}
	if size != declaredSize {
		return Entry{}, ErrSizeMismatch
	}
	if err := os.Rename(filepath.Join(s.root, "staging", entry.ID), filepath.Join(s.root, "objects", entry.ID)); err != nil {
		return Entry{}, err
	}
	now := time.Now()
	if _, err := s.db.ExecContext(ctx, `UPDATE entries SET state='complete',size_bytes=?,completed_at=?,accessed_at=?,expires_at=0 WHERE object_id=? AND state='reserved'`, size, now.UnixNano(), now.Unix(), entry.ID); err != nil {
		return Entry{}, err
	}
	entry.RepositoryID, entry.Scope, entry.Key, entry.Version, entry.Size = p.RepositoryID, scope, key, version, size
	return entry, nil
}

func (s *Store) Lookup(ctx context.Context, p Principal, key string, restoreKeys []string, version string) (Entry, bool, error) {
	if key == "" || version == "" {
		return Entry{}, false, errors.New("invalid cache key or version")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, scope := range p.ReadableScopes() {
		for _, candidate := range append([]string{key}, restoreKeys...) {
			for _, exact := range []bool{true, false} {
				var rows *sql.Rows
				var err error
				query := `SELECT id,object_id,repository_id,scope,cache_key,version,size_bytes,manifest FROM entries
				 WHERE repository_id=? AND scope=? AND version=? AND state='complete' AND `
				if exact {
					rows, err = s.db.QueryContext(ctx, query+`cache_key=? ORDER BY completed_at DESC,id DESC`, p.RepositoryID, scope, version, candidate)
				} else {
					rows, err = s.db.QueryContext(ctx, query+`substr(cache_key,1,length(?))=? ORDER BY completed_at DESC,id DESC`, p.RepositoryID, scope, version, candidate, candidate)
				}
				if err != nil {
					return Entry{}, false, err
				}
				var found []Entry
				for rows.Next() {
					var entry Entry
					var manifest string
					if err := rows.Scan(&entry.DatabaseID, &entry.ID, &entry.RepositoryID, &entry.Scope, &entry.Key, &entry.Version, &entry.Size, &manifest); err != nil {
						_ = rows.Close()
						return Entry{}, false, err
					}
					if err := json.Unmarshal([]byte(manifest), &entry.Parts); err != nil {
						_ = rows.Close()
						return Entry{}, false, err
					}
					found = append(found, entry)
				}
				if err := rows.Err(); err != nil {
					_ = rows.Close()
					return Entry{}, false, err
				}
				if err := rows.Close(); err != nil {
					return Entry{}, false, err
				}
				for _, entry := range found {
					if s.partsExist(entry) {
						_, _ = s.db.ExecContext(ctx, `UPDATE entries SET accessed_at=? WHERE object_id=?`, time.Now().Unix(), entry.ID)
						return entry, true, nil
					}
					if err := s.removeEntry(ctx, entry.ID, "objects"); err != nil {
						return Entry{}, false, err
					}
				}
			}
		}
	}
	return Entry{}, false, nil
}

func (s *Store) EntryByID(ctx context.Context, id string) (Entry, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Entry{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var entry Entry
	var manifest string
	err := s.db.QueryRowContext(ctx, `SELECT id,object_id,repository_id,scope,cache_key,version,size_bytes,manifest FROM entries
	 WHERE object_id=? AND state='complete'`, id).Scan(&entry.DatabaseID, &entry.ID, &entry.RepositoryID, &entry.Scope, &entry.Key, &entry.Version, &entry.Size, &manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, err
	}
	if err := json.Unmarshal([]byte(manifest), &entry.Parts); err != nil {
		return Entry{}, ErrIncomplete
	}
	return entry, nil
}

func (s *Store) partPath(id, blockID string) string {
	return s.partFilePath("objects", id, blockID)
}

func (s *Store) stagedPartPath(id, blockID string) string {
	return s.partFilePath("staging", id, blockID)
}

func (s *Store) partFilePath(directory, id, blockID string) string {
	var fileName string
	if err := s.db.QueryRow(`SELECT file_name FROM parts WHERE object_id=? AND block_id=?`, id, blockID).Scan(&fileName); err != nil {
		return ""
	}
	if _, err := uuid.Parse(fileName); err != nil {
		return ""
	}
	return filepath.Join(s.root, directory, id, fileName)
}

func (s *Store) partsExist(entry Entry) bool {
	for _, blockID := range entry.Parts {
		if _, err := os.Stat(s.partPath(entry.ID, blockID)); err != nil {
			return false
		}
	}
	return true
}

type partReader struct {
	*io.SectionReader
	files []*os.File
	sizes []int64
	store *Store
	id    string
	once  sync.Once
}

type partReaderAt struct {
	files []*os.File
	sizes []int64
}

func (r partReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	total := 0
	for i, file := range r.files {
		if offset >= r.sizes[i] {
			offset -= r.sizes[i]
			continue
		}
		n, err := file.ReadAt(p[total:], offset)
		total += n
		if total == len(p) {
			return total, nil
		}
		if err != nil && err != io.EOF {
			return total, err
		}
		offset = 0
	}
	return total, io.EOF
}

func (r *partReader) Close() error {
	var closeErr error
	r.once.Do(func() {
		for _, file := range r.files {
			if err := file.Close(); err != nil {
				closeErr = err
			}
		}
		r.store.mu.Lock()
		r.store.pins[r.id]--
		if r.store.pins[r.id] == 0 {
			delete(r.store.pins, r.id)
		}
		r.store.mu.Unlock()
	})
	return closeErr
}

func (s *Store) OpenEntry(entry Entry) (io.ReadSeekCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var state string
	if err := s.db.QueryRow(`SELECT state FROM entries WHERE object_id=?`, entry.ID).Scan(&state); err != nil || state != "complete" {
		return nil, ErrNotFound
	}
	reader := &partReader{store: s, id: entry.ID}
	var total int64
	for _, blockID := range entry.Parts {
		file, err := os.Open(s.partPath(entry.ID, blockID))
		if err != nil {
			for _, opened := range reader.files {
				opened.Close()
			}
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			for _, opened := range reader.files {
				opened.Close()
			}
			return nil, err
		}
		reader.files = append(reader.files, file)
		reader.sizes = append(reader.sizes, info.Size())
		total += info.Size()
	}
	if total != entry.Size {
		for _, file := range reader.files {
			file.Close()
		}
		return nil, ErrSizeMismatch
	}
	reader.SectionReader = io.NewSectionReader(partReaderAt{reader.files, reader.sizes}, 0, total)
	s.pins[entry.ID]++
	return reader, nil
}

// Sweep reclaims expired reservations, idle entries, then least-recently-used
// entries until completed bytes are below 90 percent of the configured budget.
// A reader pin keeps its object available until the transfer closes.
func (s *Store) Sweep(ctx context.Context, maxBytes int64, maxIdle time.Duration) error {
	if maxBytes <= 0 || maxIdle <= 0 {
		return errors.New("invalid cache retention policy")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	rows, err := s.db.QueryContext(ctx, `SELECT object_id,state,size_bytes,accessed_at,expires_at FROM entries ORDER BY accessed_at ASC,id ASC`)
	if err != nil {
		return err
	}
	type candidate struct {
		id, state              string
		size, accessed, expiry int64
	}
	var entries []candidate
	for rows.Next() {
		var entry candidate
		if err := rows.Scan(&entry.id, &entry.state, &entry.size, &entry.accessed, &entry.expiry); err != nil {
			_ = rows.Close()
			return err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var total int64
	for _, entry := range entries {
		if entry.state == "complete" {
			total += entry.size
		}
	}
	for _, entry := range entries {
		if entry.state == "reserved" && entry.expiry <= now.Unix() {
			if err := s.removeEntry(ctx, entry.id, "staging"); err != nil {
				return err
			}
			continue
		}
		if entry.state == "complete" && now.Sub(time.Unix(entry.accessed, 0)) >= maxIdle && s.pins[entry.id] == 0 {
			if err := s.removeEntry(ctx, entry.id, "objects"); err != nil {
				return err
			}
			total -= entry.size
		}
	}
	if total <= maxBytes {
		return nil
	}
	target := maxBytes - maxBytes/10
	for _, entry := range entries {
		if total <= target {
			break
		}
		if entry.state != "complete" || s.pins[entry.id] > 0 {
			continue
		}
		var state string
		if err := s.db.QueryRowContext(ctx, `SELECT state FROM entries WHERE object_id=?`, entry.id).Scan(&state); err != nil {
			continue
		}
		if state != "complete" {
			continue
		}
		if err := s.removeEntry(ctx, entry.id, "objects"); err != nil {
			return err
		}
		total -= entry.size
	}
	return nil
}

func (s *Store) removeEntry(ctx context.Context, id, directory string) error {
	if _, err := uuid.Parse(id); err != nil {
		return errors.New("invalid cache object ID")
	}
	if err := os.RemoveAll(filepath.Join(s.root, directory, id)); err != nil {
		return err
	}
	other := "objects"
	if directory == "objects" {
		other = "staging"
	}
	if err := os.RemoveAll(filepath.Join(s.root, other, id)); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM parts WHERE object_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM entries WHERE object_id=?`, id)
	return err
}
