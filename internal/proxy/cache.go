package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// cacheFormat is bumped whenever the on-disk layout changes; older files are ignored.
const cacheFormat = 1

// Cache records the server's answers to the requests the proxy can replay before the
// server exists (server/discover, initialize, the list methods). One file per wrapped
// command line, so changing the server version or its flags starts a fresh cache.
type Cache struct {
	path    string
	command []string

	mu      sync.Mutex
	entries map[string]json.RawMessage
}

type cacheFile struct {
	Format  int                        `json:"format"`
	Command []string                   `json:"command"`
	Entries map[string]json.RawMessage `json:"entries"`
}

// OpenCache returns the cache for command under dir. An empty dir disables caching:
// every lookup misses, so the server is started eagerly like an unwrapped one.
func OpenCache(dir string, command []string) *Cache {
	c := &Cache{command: command, entries: map[string]json.RawMessage{}}
	if dir == "" {
		return c
	}
	key, _ := json.Marshal(command)
	sum := sha256.Sum256(key)
	c.path = filepath.Join(dir, hex.EncodeToString(sum[:16])+".json")
	c.entries = c.read()
	return c
}

func (c *Cache) read() map[string]json.RawMessage {
	entries := map[string]json.RawMessage{}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return entries
	}
	var f cacheFile
	if json.Unmarshal(data, &f) != nil || f.Format != cacheFormat || f.Entries == nil {
		return entries
	}
	return f.Entries
}

func (c *Cache) Get(key string) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	return v, ok
}

// Put stores one entry and reports whether it differs from what was cached. Only a
// change rewrites the file, atomically. Other sessions wrapping the same command may
// have written meanwhile, so the file is re-read and merged first; a lost race only
// costs one future cache miss.
func (c *Cache) Put(key string, value json.RawMessage) (changed bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != "" {
		// Prefer what is on disk (it may be newer), but keep what only this session
		// knows, e.g. if the file could not be written before.
		disk := c.read()
		for k, v := range c.entries {
			if _, ok := disk[k]; !ok {
				disk[k] = v
			}
		}
		c.entries = disk
	}
	if old, ok := c.entries[key]; ok && canonical(old) == canonical(value) {
		return false, nil
	}
	c.entries[key] = value
	if c.path == "" {
		return true, nil
	}

	data, err := encode(cacheFile{Format: cacheFormat, Command: c.command, Entries: c.entries})
	if err != nil {
		return true, err
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return true, err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return true, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return true, err
	}
	if err := tmp.Close(); err != nil {
		return true, err
	}
	return true, os.Rename(tmp.Name(), c.path)
}
