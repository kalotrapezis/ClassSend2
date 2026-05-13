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
	Pinned  bool   `json:"pinned,omitempty"`
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
	sortFavorites(a.Favorites)
	a.Favorites = trimFavorites(a.Favorites)
	snap := append([]FavoriteEntry(nil), a.Favorites...)
	a.mu.Unlock()
	go saveFavorites(a.DataDir, snap)
}

// sortFavorites orders entries: pinned first, then by AddedAt desc within each group.
func sortFavorites(entries []FavoriteEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Pinned != entries[j].Pinned {
			return entries[i].Pinned
		}
		return entries[i].AddedAt > entries[j].AddedAt
	})
}

// trimFavorites enforces the cap while keeping every pinned entry. Pinned
// entries are never evicted; non-pinned tail entries are dropped first. If the
// pinned set alone exceeds the cap, all pinned entries are kept (cap is soft
// w.r.t. pins).
func trimFavorites(entries []FavoriteEntry) []FavoriteEntry {
	if len(entries) <= favoritesMaxLen {
		return entries
	}
	out := make([]FavoriteEntry, 0, len(entries))
	nonPinnedKept := 0
	pinnedTotal := 0
	for _, f := range entries {
		if f.Pinned {
			pinnedTotal++
		}
	}
	nonPinnedBudget := favoritesMaxLen - pinnedTotal
	if nonPinnedBudget < 0 {
		nonPinnedBudget = 0
	}
	// entries is already sorted (pinned first, then recency desc), so a single
	// pass keeps the most-recent non-pinned within budget.
	for _, f := range entries {
		if f.Pinned {
			out = append(out, f)
			continue
		}
		if nonPinnedKept >= nonPinnedBudget {
			continue
		}
		out = append(out, f)
		nonPinnedKept++
	}
	return out
}

// ToggleFavoritePinned flips the Pinned flag on the entry with the given value.
// Pinned entries sort above non-pinned and survive the 50-entry cap. Returns
// the new state, or false if the value wasn't found.
func (a *App) ToggleFavoritePinned(value string) (pinned bool, ok bool) {
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
		return false, false
	}
	a.Favorites[idx].Pinned = !a.Favorites[idx].Pinned
	pinned = a.Favorites[idx].Pinned
	sortFavorites(a.Favorites)
	snap := append([]FavoriteEntry(nil), a.Favorites...)
	a.mu.Unlock()
	go saveFavorites(a.DataDir, snap)
	return pinned, true
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
	path := filepath.Join(a.DataDir, favoritesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		// First launch (or file missing): seed teacher with the default classroom
		// app list ported from ClassSend v1's launcher. Student role gets nothing.
		if os.IsNotExist(err) && a.Role == RoleTeacher {
			seeded := defaultFavorites()
			a.mu.Lock()
			a.Favorites = seeded
			snap := append([]FavoriteEntry(nil), a.Favorites...)
			a.mu.Unlock()
			go saveFavorites(a.DataDir, snap)
		}
		return
	}
	var entries []FavoriteEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	sortFavorites(entries)
	a.mu.Lock()
	a.Favorites = entries
	a.mu.Unlock()
}

// defaultFavorites returns the seed list ported from ClassSend v1's app
// launcher (PREDEFINED_APPS + PREDEFINED_DOCS in client/main.js). AddedAt
// is staggered so the order in this slice is the order shown in ^N.
func defaultFavorites() []FavoriteEntry {
	paths := []string{
		// Educational / classroom apps
		`C:\Program Files\GCompris-Qt\bin\GCompris.exe`,
		`%LocalAppData%\ScratchJr\ScratchJr.exe`,
		`C:\Program Files (x86)\Scratch 3\Scratch 3.exe`,
		`C:\Program Files (x86)\Sebran\SEBRAN.EXE`,
		`C:\Program Files\TuxPaint\tuxpaint.exe`,
		`C:\Program Files\PictoBlox\PictoBlox.exe`,
		// Games
		`C:\Program Files\SuperTux\bin\supertux2.exe`,
		`C:\Program Files\SuperTuxKart 1.5\supertuxkart.exe`,
		// Browsers
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		// Office / productivity
		`C:\Program Files\ONLYOFFICE\DesktopEditors\DesktopEditors.exe`,
		`C:\Program Files\Microsoft Office\root\Office16\WINWORD.EXE`,
		`C:\Program Files (x86)\Microsoft Office\root\Office16\WINWORD.EXE`,
		`C:\Program Files\Microsoft Office\root\Office16\EXCEL.EXE`,
		`C:\Program Files (x86)\Microsoft Office\root\Office16\EXCEL.EXE`,
		`C:\Program Files\Microsoft Office\root\Office16\POWERPNT.EXE`,
		`C:\Program Files (x86)\Microsoft Office\root\Office16\POWERPNT.EXE`,
		`C:\Program Files\LibreOffice\program\swriter.exe`,
		`C:\Program Files\LibreOffice\program\scalc.exe`,
		`C:\Program Files\LibreOffice\program\simpress.exe`,
		// Media
		`C:\Program Files\kdenlive\bin\kdenlive.exe`,
		`C:\Program Files\Audacity\Audacity.exe`,
		// Windows built-ins
		`C:\Windows\System32\mspaint.exe`,
		`C:\Windows\System32\notepad.exe`,
		`C:\Windows\System32\calc.exe`,
	}
	now := time.Now().Unix()
	out := make([]FavoriteEntry, len(paths))
	for i, p := range paths {
		out[i] = FavoriteEntry{Value: p, AddedAt: now - int64(i)}
	}
	return out
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
