// Package fileBrowser 提供终端文件浏览器，支持目录导航、模糊搜索、多选与完整 TUI 渲染。
package fileBrowser

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

// Entry 代表浏览器中的一个条目。
type Entry struct {
	Name  string
	Path  string
	IsDir bool
}

// Filter 返回 true 表示条目应该被显示。
type Filter func(path string, isDir bool) bool

// ===== 模糊搜索 =====

func fuzzyMatch(query, text string) int {
	queryRunes := []rune(query)
	textRunes := []rune(text)

	if len(queryRunes) == 0 {
		return 100
	}

	queryIdx := 0
	firstMatchIndex := -1
	consecutiveMatches := 0
	maxConsecutive := 0

	for i, textRune := range textRunes {
		if unicodeFold(textRune) == unicodeFold(queryRunes[queryIdx]) {
			if firstMatchIndex == -1 {
				firstMatchIndex = i
			}

			consecutiveMatches++
			if consecutiveMatches > maxConsecutive {
				maxConsecutive = consecutiveMatches
			}

			queryIdx++
			if queryIdx == len(queryRunes) {
				break
			}
		} else {
			consecutiveMatches = 0
		}
	}

	if queryIdx < len(queryRunes) {
		return 0
	}

	score := 100
	lastMatchIndex := firstMatchIndex + queryIdx - 1
	matchSpread := lastMatchIndex - firstMatchIndex
	if matchSpread > 0 {
		spreadPenalty := (matchSpread * 10) / len(textRunes)
		score -= spreadPenalty
	}
	if maxConsecutive > 1 {
		score += maxConsecutive * 5
	}
	if firstMatchIndex == 0 {
		score += 20
	}
	if len(textRunes) < 50 {
		score += (50 - len(textRunes)) / 5
	}
	if score < 1 {
		score = 1
	}
	return score
}

func unicodeFold(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return unicode.ToLower(r)
}

// ===== Browser =====

type Browser struct {
	root     string
	filter   Filter
	selected map[string]bool

	entries     []Entry
	currentPath string
	cursor      int
	offset      int
	pathHistory map[string]int
	lastEntered string

	isSearching bool
	searchQuery string

	globalCache  []string
	filteredResults []string

	dirSelCache map[string]bool
}

// New 创建文件浏览器。root 为根路径（不可退到其父目录之外）。
// filter 为 nil 则接受所有条目。selected 为初始选中的路径集合。
func New(root string, filter Filter, selected map[string]bool) *Browser {
	if selected == nil {
		selected = make(map[string]bool)
	}
	return &Browser{
		root:         root,
		filter:       filter,
		selected:     selected,
		currentPath:  root,
		pathHistory:  make(map[string]int),
		dirSelCache:  make(map[string]bool),
	}
}

// ===== 导航 =====

func (b *Browser) Enter() []string {
	if b.isSearching {
		return nil
	}
	if b.searchQuery != "" {
		if b.cursor < len(b.filteredResults) {
			path := b.filteredResults[b.cursor]
			info, err := os.Stat(path)
			if err == nil && info.IsDir() {
				b.scanDirectory(path)
				return nil
			}
		}
		return nil
	}
	if b.cursor < len(b.entries) && b.entries[b.cursor].IsDir {
		b.lastEntered = b.entries[b.cursor].Name
		b.scanDirectory(b.entries[b.cursor].Path)
	}
	return nil
}

func (b *Browser) Exit() []string {
	currentAbs, _ := filepath.Abs(b.currentPath)
	rootAbs, _ := filepath.Abs(b.root)
	if currentAbs == rootAbs {
		return nil
	}
	parent := filepath.Dir(b.currentPath)
	b.scanDirectory(parent)
	if b.lastEntered != "" {
		for i, e := range b.entries {
			if e.Name == b.lastEntered && e.IsDir {
				b.cursor = i
				break
			}
		}
		b.lastEntered = ""
	}
	return nil
}

func (b *Browser) MoveUp() {
	total := b.visibleCount()
	if total > 0 {
		b.cursor = (b.cursor - 1 + total) % total
	}
}

func (b *Browser) MoveDown() {
	total := b.visibleCount()
	if total > 0 {
		b.cursor = (b.cursor + 1) % total
	}
}

// ===== 搜索 =====

func (b *Browser) BeginSearch()    { b.isSearching = true }
func (b *Browser) IsSearching() bool { return b.isSearching }
func (b *Browser) SearchQuery() string { return b.searchQuery }

func (b *Browser) AppendSearch(r rune) {
	if r >= 32 {
		b.searchQuery += string(r)
		b.filterSongs()
	}
}

func (b *Browser) BackspaceSearch() {
	runes := []rune(b.searchQuery)
	if len(runes) > 0 {
		b.searchQuery = string(runes[:len(runes)-1])
		b.filterSongs()
	}
}

func (b *Browser) CommitSearch() { b.isSearching = false }

func (b *Browser) CancelSearch() {
	b.isSearching = false
	b.searchQuery = ""
	b.filterSongs()
}

// ===== 选择 =====

// ToggleCurrent 切换当前光标条目的选择状态，返回受影响的路径列表。
func (b *Browser) ToggleCurrent() []string {
	if b.searchQuery != "" {
		return b.toggleSearchResult()
	}
	if b.cursor < len(b.entries) {
		return b.toggleEntry(b.entries[b.cursor])
	}
	return nil
}

func (b *Browser) toggleSearchResult() []string {
	if b.cursor >= len(b.filteredResults) {
		return nil
	}
	path := b.filteredResults[b.cursor]
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if info.IsDir() {
		songs := b.collectSongs(path)
		allSel := true
		for _, sp := range songs {
			if !b.selected[sp] {
				allSel = false
				break
			}
		}
		if len(songs) == 0 {
			allSel = false
		}
		var affected []string
		for _, sp := range songs {
			if allSel {
				b.deselectAndRemove(sp)
			} else if !b.selected[sp] {
				b.selectAndAdd(sp)
			}
			affected = append(affected, sp)
		}
		b.cursorAdvance()
		b.dirSelCache = make(map[string]bool)
		return affected
	} else {
		b.doToggle(path)
		b.cursorAdvance()
		return []string{path}
	}
}

func (b *Browser) toggleEntry(e Entry) []string {
	if !e.IsDir {
		b.doToggle(e.Path)
		b.cursorAdvance()
		return []string{e.Path}
	}
	songs := b.collectSongs(e.Path)
	allSel := true
	for _, sp := range songs {
		if !b.selected[sp] {
			allSel = false
			break
		}
	}
	if len(songs) == 0 {
		allSel = false
	}
	var affected []string
	for _, sp := range songs {
		if allSel {
			b.deselectAndRemove(sp)
		} else {
			if !b.selected[sp] {
				b.selectAndAdd(sp)
			}
		}
		affected = append(affected, sp)
	}
	b.cursorAdvance()
	b.dirSelCache = make(map[string]bool)
	return affected
}

func (b *Browser) doToggle(path string) {
	if b.selected[path] {
		b.deselectAndRemove(path)
	} else {
		b.selectAndAdd(path)
	}
}

func (b *Browser) selectAndAdd(path string) {
	b.selected[path] = true
	b.dirSelCache = make(map[string]bool)
}

func (b *Browser) deselectAndRemove(path string) {
	delete(b.selected, path)
	b.dirSelCache = make(map[string]bool)
}

// ToggleAll 全选或取消全选当前视图中的所有条目，返回受影响的路径列表。
func (b *Browser) ToggleAll() []string {
	var allSongs []string
	if b.searchQuery != "" {
		for _, path := range b.filteredResults {
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.IsDir() {
				allSongs = append(allSongs, b.collectSongs(path)...)
			} else {
				allSongs = append(allSongs, path)
			}
		}
	} else {
		for _, e := range b.entries {
			if !e.IsDir {
				allSongs = append(allSongs, e.Path)
			} else {
				allSongs = append(allSongs, b.collectSongs(e.Path)...)
			}
		}
	}
	if len(allSongs) == 0 {
		return nil
	}

	allCurrentlySelected := true
	for _, sp := range allSongs {
		if !b.selected[sp] {
			allCurrentlySelected = false
			break
		}
	}

	if allCurrentlySelected {
		for _, sp := range allSongs {
			delete(b.selected, sp)
		}
	} else {
		for _, sp := range allSongs {
			b.selected[sp] = true
		}
	}
	b.dirSelCache = make(map[string]bool)
	return allSongs
}

// Deselect 取消选中指定路径。
func (b *Browser) Deselect(path string) {
	delete(b.selected, path)
}

// Selected 返回当前选中路径的副本。
func (b *Browser) Selected() map[string]bool {
	m := make(map[string]bool, len(b.selected))
	for k, v := range b.selected {
		m[k] = v
	}
	return m
}

// ===== 查询 =====

func (b *Browser) CurrentPath() string { return b.currentPath }
func (b *Browser) Entries() []Entry     { return b.entries }

// ===== 内部 =====

func (b *Browser) cursorAdvance() {
	if b.searchQuery != "" {
		if b.cursor < len(b.filteredResults)-1 {
			b.cursor++
		}
	} else {
		if b.cursor < len(b.entries)-1 {
			b.cursor++
		}
	}
}

func (b *Browser) visibleCount() int {
	if b.searchQuery != "" {
		return len(b.filteredResults)
	}
	return len(b.entries)
}

func (b *Browser) scanDirectory(path string) {
	if b.currentPath != "" {
		b.pathHistory[b.currentPath] = b.cursor
	}
	b.entries = make([]Entry, 0)
	b.currentPath = path

	if savedCursor, ok := b.pathHistory[path]; ok {
		b.cursor = savedCursor
	} else {
		b.cursor = 0
	}

	files, err := os.ReadDir(path)
	if err != nil {
		return
	}

	for _, file := range files {
		info, err := file.Info()
		if err != nil {
			continue
		}

		isDir := info.IsDir()
		fullPath := filepath.Join(path, file.Name())

		if info.Mode()&os.ModeSymlink != 0 {
			targetInfo, err := os.Stat(fullPath)
			if err != nil {
				continue
			}
			isDir = targetInfo.IsDir()
		}

		if b.filter != nil && !b.filter(fullPath, isDir) {
			continue
		}

		b.entries = append(b.entries, Entry{
			Name:  file.Name(),
			Path:  fullPath,
			IsDir: isDir,
		})
	}

	sort.SliceStable(b.entries, func(i, j int) bool {
		if b.entries[i].IsDir != b.entries[j].IsDir {
			return b.entries[i].IsDir
		}
		return strings.ToLower(b.entries[i].Name) < strings.ToLower(b.entries[j].Name)
	})
	b.offset = 0
}

func (b *Browser) ensureGlobalCache() {
	if b.globalCache != nil {
		return
	}
	var cache []string
	filepath.Walk(b.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if path == b.root {
			return nil
		}
		isDir := info.IsDir()
		if info.Mode()&os.ModeSymlink != 0 {
			stat, e := os.Stat(path)
			if e != nil {
				return nil
			}
			isDir = stat.IsDir()
		}
		if b.filter != nil && !b.filter(path, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		if isDir {
			cache = append(cache, path)
		}
		return nil
	})
	// Append audio files
	filepath.Walk(b.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if path == b.root {
			return nil
		}
		isDir := info.IsDir()
		if info.Mode()&os.ModeSymlink != 0 {
			stat, e := os.Stat(path)
			if e != nil {
				return nil
			}
			isDir = stat.IsDir()
		}
		if isDir {
			return nil
		}
		if b.filter != nil && !b.filter(path, false) {
			return nil
		}
		cache = append(cache, path)
		return nil
	})
	b.globalCache = cache
}

func (b *Browser) filterSongs() {
	if b.searchQuery == "" {
		b.filteredResults = nil
		b.scanDirectory(b.currentPath)
		return
	}

	b.ensureGlobalCache()

	type scored struct {
		path  string
		score int
	}
	var items []scored

	for _, path := range b.globalCache {
		score := fuzzyMatch(b.searchQuery, path)
		if score > 0 {
			items = append(items, scored{path, score})
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		return items[i].score > items[j].score
	})

	b.filteredResults = make([]string, len(items))
	for i, item := range items {
		b.filteredResults[i] = item.path
	}
	b.cursor = 0
	b.offset = 0
}

func (b *Browser) collectSongs(dir string) []string {
	var songs []string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if path == dir {
			return nil
		}
		isDir := info.IsDir()
		if info.Mode()&os.ModeSymlink != 0 {
			stat, e := os.Stat(path)
			if e != nil {
				return nil
			}
			isDir = stat.IsDir()
		}
		if isDir {
			return nil
		}
		if b.filter != nil && !b.filter(path, false) {
			return nil
		}
		songs = append(songs, path)
		return nil
	})
	return songs
}

func (b *Browser) isDirPartiallySelected(dirPath string) bool {
	if cached, ok := b.dirSelCache[dirPath]; ok {
		return cached
	}
	result := b.checkDirSelected(dirPath)
	b.dirSelCache[dirPath] = result
	return result
}

func (b *Browser) checkDirSelected(dirPath string) bool {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return false
	}
	for _, file := range entries {
		fullPath := filepath.Join(dirPath, file.Name())
		info, err := file.Info()
		if err != nil {
			continue
		}
		isDir := info.IsDir()
		if info.Mode()&os.ModeSymlink != 0 {
			stat, e := os.Stat(fullPath)
			if e != nil {
				continue
			}
			isDir = stat.IsDir()
		}
		if isDir {
			if b.checkDirSelected(fullPath) {
				return true
			}
		} else {
			if b.selected[fullPath] {
				return true
			}
		}
	}
	return false
}

// ===== 渲染 =====

// Render 输出当前页面的完整 ANSI 渲染到终端。
func (b *Browser) Render(w, h int) {
	fmt.Print("\x1b[2J\x1b[3J\x1b[H")

	title := "Library"
	titleX := (w - len(title)) / 2
	if titleX < 1 {
		titleX = 1
	}
	fmt.Printf("\x1b[1;%dH\x1b[1m%s\x1b[0m", titleX, title)

	listHeight := h - 4
	if listHeight < 1 {
		listHeight = 1
	}

	// Footer
	b.renderFooter(w, h)

	// Content
	if b.searchQuery != "" {
		b.renderSearchResults(w, h, listHeight)
	} else {
		b.renderDirectory(w, h, listHeight)
	}

	// Scrollbar
	b.renderScrollbar(w, h, listHeight)
}

func (b *Browser) renderFooter(w, h int) {
	var text string
	if b.isSearching || b.searchQuery != "" {
		text = fmt.Sprintf("Search: %s", b.searchQuery)
	} else {
		text = fmt.Sprintf("Path: %s", filepath.Base(b.currentPath))
	}

	if runewidth.StringWidth(text) > w {
		text = "..." + text[len(text)-w+3:]
	}
	footerX := (w - runewidth.StringWidth(text)) / 2
	if footerX < 1 {
		footerX = 1
	}
	fmt.Printf("\x1b[%d;%dH\x1b[90m%s\x1b[0m", h, footerX, text)
	if b.isSearching {
		cursorX := footerX + len("Search: ") + runewidth.StringWidth(b.searchQuery)
		if cursorX <= w {
			fmt.Printf("\x1b[%d;%dH█", h, cursorX)
		}
	}
}

func (b *Browser) renderSearchResults(w, h, listHeight int) {
	list := b.filteredResults
	total := len(list)

	if b.cursor < b.offset {
		b.offset = b.cursor
	}
	if b.cursor >= b.offset+listHeight {
		b.offset = b.cursor - listHeight + 1
	}

	for i := 0; i < listHeight; i++ {
		idx := b.offset + i
		if idx >= total {
			break
		}
		path := list[idx]
		info, err := os.Stat(path)
		isDir := err == nil && info.IsDir()
		isCursor := idx == b.cursor

		isSelected := b.selected[path]
		if isDir && !isSelected {
			isSelected = b.isDirPartiallySelected(path)
		}

		b.renderEntryLine(i+3, w, isSelected, isCursor, isDir, path, path)
	}
}

func (b *Browser) renderDirectory(w, h, listHeight int) {
	total := len(b.entries)

	if b.cursor < b.offset {
		b.offset = b.cursor
	}
	if b.cursor >= b.offset+listHeight {
		b.offset = b.cursor - listHeight + 1
	}

	for i := 0; i < listHeight; i++ {
		idx := b.offset + i
		if idx >= total {
			break
		}
		e := b.entries[idx]
		isCursor := idx == b.cursor

		isSelected := b.selected[e.Path]
		if e.IsDir && !isSelected {
			isSelected = b.isDirPartiallySelected(e.Path)
		}

		var name string
		if e.IsDir {
			name = e.Name + "/"
		} else {
			name = e.Name
		}
		prefix := "  "
		if e.IsDir {
			if isSelected {
				prefix = "✓ "
			} else {
				prefix = "▸ "
			}
		} else if isSelected {
			prefix = "✓ "
		}

		b.renderEntryLine(i+3, w, isSelected, isCursor, e.IsDir, prefix+name, e.Path)
	}
}

func (b *Browser) renderEntryLine(row, w int, isSelected, isCursor, isDir bool, display string, _ string) {
	line := display
	if runewidth.StringWidth(line) > w-1 {
		for runewidth.StringWidth(line) > w-1 && len(line) > 0 {
			line = line[:len(line)-1]
		}
	}

	var style string
	if isSelected {
		style = "\x1b[0m\x1b[32m"
	} else {
		style = "\x1b[0m"
	}
	if isCursor {
		style += "\x1b[7m"
	}
	fmt.Printf("\x1b[%d;1H\x1b[K%s%s\x1b[0m", row, style, line)
}

func (b *Browser) renderScrollbar(w, h, listHeight int) {
	total := b.visibleCount()
	if total <= listHeight {
		return
	}

	thumbSize := listHeight * listHeight / total
	if thumbSize < 1 {
		thumbSize = 1
	}
	scrollRange := total - listHeight
	thumbRange := listHeight - thumbSize

	thumbStart := 0
	if scrollRange > 0 {
		thumbStart = b.offset * thumbRange / scrollRange
	}

	for i := 0; i < listHeight; i++ {
		if i >= thumbStart && i < thumbStart+thumbSize {
			fmt.Printf("\x1b[%d;%dH┃", i+3, w)
		} else {
			fmt.Printf("\x1b[%d;%dH│", i+3, w)
		}
	}
}

// ===== 终端尺寸 =====

func TermSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80, 24
	}
	return w, h
}

// ===== 音频文件判断辅助 =====

func IsAudioExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".flac" || ext == ".mp3" || ext == ".wav" || ext == ".ogg"
}
