package lua

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BenLocal/exams/internal/collector"
)

// LoadResult reports what a directory scan produced.
type LoadResult struct {
	Sources []collector.Collector
	// Errors are per-script problems. They are returned rather than aborting,
	// so one malformed script does not take the service down.
	Errors []error
}

// LoadDir compiles every *.lua file in dir.
//
// A missing directory is not an error: Lua plugins are optional, and a fresh
// checkout has none.
func LoadDir(dir string, loc *time.Location, log *slog.Logger) (LoadResult, error) {
	var res LoadResult
	if strings.TrimSpace(dir) == "" {
		return res, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, fmt.Errorf("read plugins dir %s: %w", dir, err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		// Skip editor backups and dotfiles so a stray .lua~ is not loaded.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	// Track keys claimed in this scan as well as in the registry, so two
	// scripts cannot silently shadow each other.
	seen := map[string]string{}

	for _, name := range files {
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("%s: %w", path, err))
			continue
		}

		source, err := compile(path, name, string(src), loc, log)
		if err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}

		if existing, ok := collector.Get(source.Key()); ok {
			res.Errors = append(res.Errors, fmt.Errorf(
				"%s: key %q is already used by the built-in collector %q\n"+
					"  Change `key` in the script, or disable the built-in source from the sources page",
				path, source.Key(), existing.Name()))
			continue
		}
		if other, ok := seen[source.Key()]; ok {
			res.Errors = append(res.Errors, fmt.Errorf(
				"%s: key %q is already used by %s", path, source.Key(), other))
			continue
		}
		seen[source.Key()] = path

		res.Sources = append(res.Sources, source)
	}
	return res, nil
}

// compile turns one script file into a Source.
func compile(path, fileName, src string, loc *time.Location, log *slog.Logger) (*Source, error) {
	// A throwaway state for the load-time metadata read.
	L := newState(nil)
	defer L.Close()

	proto, err := loadChunk(L, path, src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	meta, hasDetail, err := readMeta(L, proto, path)
	if err != nil {
		return nil, err
	}

	if meta.key == "" {
		// Falling back to the filename keeps trivial scripts terse, but the
		// key is the database foreign key, so it is spelled out in the log.
		meta.key = strings.TrimSuffix(fileName, ".lua")
	}
	if meta.name == "" {
		meta.name = meta.key
	}
	if meta.baseURL == "" {
		return nil, fmt.Errorf(
			"%s: `base_url` is required — relative links and ctx:get() resolve against it",
			path)
	}

	s := &Source{
		path:      path,
		key:       meta.key,
		name:      meta.name,
		baseURL:   strings.TrimRight(meta.baseURL, "/"),
		category:  meta.category,
		region:    meta.region,
		proto:     proto,
		hasDetail: hasDetail,
		loc:       loc,
		log:       log,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	// The key is a database foreign key: changing it orphans every scraped row.
	if !validKey(s.key) {
		return nil, fmt.Errorf(
			"%s: key %q is not usable — use letters, digits, dash and underscore only",
			path, s.key)
	}
	return s, nil
}

// validKey reports whether a key is safe to use as a source identifier.
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// CompileFile compiles a single script. Exported for tests.
func CompileFile(path string, loc *time.Location, log *slog.Logger) (*Source, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return compile(path, filepath.Base(path), string(src), loc, log)
}
