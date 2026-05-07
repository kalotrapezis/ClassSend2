package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FavoriteEntry is one saved target (URL or absolute file path) the teacher
// has push-opened or attached recently. Kept dead simple: the value is the
// only field the user sees, AddedAt drives recency ordering.
type FavoriteEntry struct {
	Value   string `json:"value"`
	AddedAt int64  `json:"added_at"`
}

const (
	favoritesFile   = "favorites.json"
	favoritesMaxLen = 50 // cap; oldest get dropped on overflow
)

var favoritesMu sync.Mutex

// AddFavorite records a URL or file path the teacher just used. Duplicates
// move to the top instead of stacking. Empty / non-teacher / non-actionable
// values are silently skipped — keeps callers from sprinkling guards.
func (a *App) AddFavorite(value string) {
	value = strings.TrimSpace(value)
	if value == "" || a.Role != RoleTeacher {
		return
	}
	a.mu.Lock()
	now := time.Now().Unix()
	// Move-to-front if already present
	found := -1
	for i, f := range a.Favorites {
		if f.Value == value {
			found = i
			break
		}
	}
	if found >= 0 {
		a.Favorites[found].AddedAt = now
	} else {
		a.Favorites = append(a.Favorites, FavoriteEntry{Value: value, AddedAt: now})
	}
	// Sort newest-first, trim to cap
	sort.SliceStable(a.Favorites, func(i, j int) bool {
		return a.Favorites[i].AddedAt > a.Favorites[j].AddedAt
	})
	if len(a.Favorites) > favoritesMaxLen {
		a.Favorites = a.Favorites[:favoritesMaxLen]
	}
	snap := append([]FavoriteEntry(nil), a.Favorites...)
	a.mu.Unlock()
	go saveFavorites(a.DataDir, snap)
}

// RemoveFavorite drops one entry by exact value match. Returns true if removed.
func (a *App) RemoveFavorite(value string) bool {
	a.mu.Lock()
	idx := -1
	for i, f := range a.Favorites {
		if f.Value == value {
			idx = i
			break
		}
	}
	if idx < 0 {
		a.mu.Unlock()
		return false
	}
	a.Favorites = append(a.Favorites[:idx], a.Favorites[idx+1:]...)
	snap := append([]FavoriteEntry(nil), a.Favorites...)
	a.mu.Unlock()
	go saveFavorites(a.DataDir, snap)
	return true
}

// FavoritesSnapshot returns a copy of the current favorites in newest-first
// order, safe for the TUI to iterate without holding App.mu.
func (a *App) FavoritesSnapshot() []FavoriteEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]FavoriteEntry, len(a.Favorites))
	copy(out, a.Favorites)
	return out
}

func (a *App) loadFavorites() {
	data, err := os.ReadFile(filepath.Join(a.DataDir, favoritesFile))
	if err != nil {
		return
	}
	var entries []FavoriteEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	a.mu.Lock()
	a.Favorites = entries
	a.mu.Unlock()
}

// saveFavorites is package-level (not a method) so the goroutine launched
// from AddFavorite/RemoveFavorite doesn't keep a reference to App.mu.
// favoritesMu serialises writes against each other so concurrent callers
// can't corrupt the JSON file.
func saveFavorites(dataDir string, entries []FavoriteEntry) {
	favoritesMu.Lock()
	defer favoritesMu.Unlock()
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(dataDir, favoritesFile), data, 0644)
}
