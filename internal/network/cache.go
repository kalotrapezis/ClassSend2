package network

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxIPHistory = 5

type CacheEntry struct {
	MAC       string    `json:"mac"`
	LastIP    string    `json:"last_ip"`
	IPHistory []string  `json:"ip_history"` // last N IPs, index 0 = most recent
	Nickname  string    `json:"nickname"`
	Hostname  string    `json:"hostname"`
	LastSeen  time.Time `json:"last_seen"`
}

type MACCache struct {
	mu      sync.RWMutex
	entries map[string]*CacheEntry // keyed by MAC
	path    string
}

func NewMACCache(dataDir string) *MACCache {
	c := &MACCache{
		entries: make(map[string]*CacheEntry),
		path:    filepath.Join(dataDir, "mac_cache.json"),
	}
	c.load()
	return c
}

func (c *MACCache) Get(mac string) (*CacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[mac]
	return e, ok
}

// Upsert updates or creates an entry for a MAC address
func (c *MACCache) Upsert(mac, ip, nickname, hostname string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.entries[mac]
	if ok {
		existing.LastSeen = time.Now()
		if ip != "" {
			existing.LastIP = ip
			existing.IPHistory = prependIP(existing.IPHistory, ip)
		}
		if nickname != "" {
			existing.Nickname = nickname
		}
		if hostname != "" {
			existing.Hostname = hostname
		}
	} else {
		history := []string{}
		if ip != "" {
			history = []string{ip}
		}
		c.entries[mac] = &CacheEntry{
			MAC:       mac,
			LastIP:    ip,
			IPHistory: history,
			Nickname:  nickname,
			Hostname:  hostname,
			LastSeen:  time.Now(),
		}
	}
	c.save()
}

// prependIP adds ip to the front of history, deduplicates, and caps at maxIPHistory
func prependIP(history []string, ip string) []string {
	out := []string{ip}
	for _, h := range history {
		if h != ip {
			out = append(out, h)
		}
		if len(out) >= maxIPHistory {
			break
		}
	}
	return out
}

// All returns a snapshot of all entries — used for fast-path probing on startup
func (c *MACCache) All() []CacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]CacheEntry, 0, len(c.entries))
	for _, e := range c.entries {
		result = append(result, *e)
	}
	return result
}

// SetNickname lets the teacher rename a known device
func (c *MACCache) SetNickname(mac, nickname string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[mac]; ok {
		e.Nickname = nickname
		c.save()
	}
}

func (c *MACCache) load() {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var entries []CacheEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	for i := range entries {
		c.entries[entries[i].MAC] = &entries[i]
	}
}

func (c *MACCache) save() {
	entries := make([]CacheEntry, 0, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, *e)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(c.path), 0755)
	os.WriteFile(c.path, data, 0644)
}
