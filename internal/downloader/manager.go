package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ngenohkevin/debrid-vault-api/internal/config"
	"github.com/ngenohkevin/debrid-vault-api/internal/debrid"
	"github.com/ngenohkevin/debrid-vault-api/internal/media"
)

// PostMoveFunc is called after a file is moved to its final location.
// Receives the download ID and the final file path.
type PostMoveFunc func(id, finalPath string)

type Manager struct {
	cfg              *config.Config
	providers        map[string]debrid.Provider
	engine           *DownloadEngine
	downloads        map[string]*DownloadItem
	cancels          map[string]context.CancelFunc
	mu               sync.RWMutex
	subs             map[chan Event]struct{}
	subsMu           sync.RWMutex
	sem              chan struct{}
	activeCount      atomic.Int32
	historyFile      string
	completedSources map[string]bool
	completedFile    string
	postMoveHook     PostMoveFunc
	metaStore        map[string]map[string]string // id -> metadata key-value pairs
}

// SetPostMoveHook sets a callback to run after a file is moved to its final location.
func (m *Manager) SetPostMoveHook(fn PostMoveFunc) {
	m.postMoveHook = fn
}

// SetMeta stores metadata for a download item (used for music tagging).
func (m *Manager) SetMeta(id string, meta map[string]string) {
	m.mu.Lock()
	if m.metaStore == nil {
		m.metaStore = make(map[string]map[string]string)
	}
	m.metaStore[id] = meta
	m.mu.Unlock()
}

// GetMeta retrieves stored metadata for a download item.
func (m *Manager) GetMeta(id string) map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.metaStore == nil {
		return nil
	}
	return m.metaStore[id]
}

// ClearMeta removes stored metadata for a download item.
func (m *Manager) ClearMeta(id string) {
	m.mu.Lock()
	if m.metaStore != nil {
		delete(m.metaStore, id)
	}
	m.mu.Unlock()
}

func NewManager(cfg *config.Config, providers map[string]debrid.Provider) *Manager {
	m := &Manager{
		cfg:              cfg,
		providers:        providers,
		engine:           NewDownloadEngine(cfg.MaxSegmentsPerFile, cfg.SpeedLimitMbps),
		downloads:        make(map[string]*DownloadItem),
		cancels:          make(map[string]context.CancelFunc),
		subs:             make(map[chan Event]struct{}),
		sem:              make(chan struct{}, cfg.MaxConcurrentDownloads),
		historyFile:      filepath.Join(cfg.DownloadDir, ".history.json"),
		completedSources: make(map[string]bool),
		completedFile:    filepath.Join(cfg.DownloadDir, ".completed.json"),
	}
	m.loadHistory()
	m.loadCompletedSources()
	return m
}

// provider returns the debrid provider by name, falling back to the first available.
func (m *Manager) provider(name string) debrid.Provider {
	if p, ok := m.providers[name]; ok {
		return p
	}
	// Fall back to first available provider
	for _, p := range m.providers {
		return p
	}
	return nil
}

func (m *Manager) Engine() *DownloadEngine {
	return m.engine
}

// SetMaxConcurrent updates the concurrency semaphore for new downloads.
// In-flight downloads continue with the old limit; new downloads use the new one.
func (m *Manager) SetMaxConcurrent(n int) {
	if n < 1 {
		n = 1
	}
	m.mu.Lock()
	m.sem = make(chan struct{}, n)
	m.cfg.MaxConcurrentDownloads = n
	m.mu.Unlock()
}

// SetMaxSegments updates the max segments per file for future downloads.
func (m *Manager) SetMaxSegments(n int) {
	if n < 1 {
		n = 1
	}
	m.engine.maxSegments = n
	m.cfg.MaxSegmentsPerFile = n
}

// AddTrackedDownload adds an externally-created download item to the manager for tracking.
// Used by Tidal provider which manages its own download logic.
func (m *Manager) AddTrackedDownload(item *DownloadItem, cancel context.CancelFunc) {
	m.mu.Lock()
	m.downloads[item.ID] = item
	m.cancels[item.ID] = cancel
	m.mu.Unlock()
	m.emit(Event{Type: "added", Data: *item})
}

// UpdateItemStatus updates the status of a download item and emits an event.
func (m *Manager) UpdateItemStatus(id string, status Status) {
	m.mu.Lock()
	item, ok := m.downloads[id]
	if ok {
		item.Status = status
	}
	m.mu.Unlock()
	if ok {
		m.emit(Event{Type: "progress", Data: *item})
	}
}

// SetItemError marks a download as failed with an error message.
func (m *Manager) SetItemError(id, errMsg string) {
	m.mu.Lock()
	item, ok := m.downloads[id]
	if ok {
		item.Status = StatusError
		item.Error = errMsg
		item.Speed = 0
		item.ETA = 0
	}
	m.mu.Unlock()
	if ok {
		m.emit(Event{Type: "error", Data: *item})
		m.saveHistory()
	}
}

// MoveCompletedFile moves a downloaded file from staging to its final destination.
// Used by external providers (Tidal) that handle their own download logic.
func (m *Manager) MoveCompletedFile(ctx context.Context, item *DownloadItem, stagingPath string) {
	m.updateStatus(item, StatusMoving, "")

	category := item.Category
	var finalDir string
	switch category {
	case CategoryMovies:
		finalDir = m.cfg.MoviesDir
	case CategoryTVShows:
		finalDir = m.cfg.TVShowsDir
	case CategoryMusic:
		finalDir = m.cfg.MusicDir
	default:
		finalDir = m.cfg.MoviesDir
	}

	if item.Folder != "" {
		finalDir = filepath.Join(finalDir, item.Folder)
	}

	if err := os.MkdirAll(finalDir, 0755); err != nil {
		m.setError(item, fmt.Sprintf("Failed to create dir: %v", err))
		return
	}

	finalPath := filepath.Join(finalDir, item.Name)

	// Remove existing file if present
	if _, err := os.Stat(finalPath); err == nil {
		log.Printf("Overwriting existing: %s", finalPath)
		os.Remove(finalPath)
	}

	// Try rename first (same filesystem), fallback to copy+delete
	if err := os.Rename(stagingPath, finalPath); err != nil {
		if err := copyFile(stagingPath, finalPath); err != nil {
			m.setError(item, fmt.Sprintf("Failed to move: %v", err))
			return
		}
		os.Remove(stagingPath)
	}

	now := time.Now()
	m.mu.Lock()
	item.Status = StatusCompleted
	item.Progress = 1.0
	item.FilePath = finalPath
	item.CompletedAt = &now
	item.Speed = 0
	item.ETA = 0
	m.markCompleted(item.Source)
	m.mu.Unlock()

	m.emit(Event{Type: "completed", Data: *item})
	m.saveHistory()
	log.Printf("Download complete: %s -> %s", item.Name, finalPath)

	// Post-move hook (metadata tagging)
	if m.postMoveHook != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("postMoveHook panic for %s: %v", item.Name, r)
				}
			}()
			m.postMoveHook(item.ID, finalPath)
		}()
	}
}

// AcquireSlot blocks until a concurrency slot is available.
func (m *Manager) AcquireSlot(ctx context.Context) {
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
	}
}

// ReleaseSlot releases a concurrency slot.
func (m *Manager) ReleaseSlot() {
	<-m.sem
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	for id, cancel := range m.cancels {
		cancel()
		if item, ok := m.downloads[id]; ok {
			if item.Status == StatusDownloading || item.Status == StatusResolving || item.Status == StatusMoving || item.Status == StatusQueued {
				// Mark as paused so loadHistory can auto-resume on restart
				item.Status = StatusPaused
				item.Speed = 0
				item.ETA = 0
			}
		}
	}
	m.mu.Unlock()
	m.saveHistory()
}

func (m *Manager) Subscribe() chan Event {
	ch := make(chan Event, 64)
	m.subsMu.Lock()
	m.subs[ch] = struct{}{}
	m.subsMu.Unlock()
	return ch
}

func (m *Manager) Unsubscribe(ch chan Event) {
	m.subsMu.Lock()
	delete(m.subs, ch)
	m.subsMu.Unlock()
	close(ch)
}

func (m *Manager) emit(event Event) {
	m.subsMu.RLock()
	defer m.subsMu.RUnlock()
	for ch := range m.subs {
		select {
		case ch <- event:
		default:
		}
	}
}

func (m *Manager) GetDownloads() []DownloadItem {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]DownloadItem, 0, len(m.downloads))
	for _, item := range m.downloads {
		items = append(items, *item)
	}
	return items
}

func (m *Manager) GetDownload(id string) (*DownloadItem, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.downloads[id]
	if !ok {
		return nil, fmt.Errorf("download not found: %s", id)
	}
	cp := *item
	return &cp, nil
}

func (m *Manager) GetDownloadsByGroup(groupID string) []DownloadItem {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var items []DownloadItem
	for _, item := range m.downloads {
		if item.GroupID == groupID {
			items = append(items, *item)
		}
	}
	return items
}

// CheckResumable checks if a download can be resumed later.
// Returns an error describing why it can't be resumed.
func (m *Manager) CheckResumable(id string) error {
	m.mu.RLock()
	item, ok := m.downloads[id]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("download not found: %s", id)
	}
	status := item.Status
	hasURL := item.DownloadURL != ""
	hasSource := item.Source != ""
	name := item.Name
	m.mu.RUnlock()

	// Completed/cancelled items can't be scheduled
	if status == StatusCompleted || status == StatusCancelled {
		return fmt.Errorf("download already %s", status)
	}

	// Need either a download URL (for resume) or a source (for re-unrestrict)
	if !hasURL && !hasSource {
		return fmt.Errorf("no download URL or source to resume from")
	}

	// Allow pausing resolving items — they'll be cancelled and can be retried later
	if status == StatusResolving || status == StatusQueued {
		return nil
	}
	if name == "" || name == "Resolving magnet..." || name == "Resolving link..." {
		return fmt.Errorf("download hasn't resolved yet, wait for it to start")
	}

	return nil
}

func (m *Manager) CancelDownload(id string) error {
	m.mu.Lock()
	cancel, ok := m.cancels[id]
	if ok {
		cancel()
		delete(m.cancels, id)
	}
	item, exists := m.downloads[id]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("download not found: %s", id)
	}
	if item.Status == StatusDownloading || item.Status == StatusResolving || item.Status == StatusPending || item.Status == StatusQueued {
		item.Status = StatusCancelled
	}
	name := item.Name
	m.mu.Unlock()
	m.emit(Event{Type: "cancelled", Data: *item})
	m.saveHistory()

	// Clean up staging files
	m.cleanStagingFile(name)
	return nil
}

func (m *Manager) PauseDownload(id string) error {
	m.mu.Lock()
	cancel, ok := m.cancels[id]
	item, exists := m.downloads[id]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("download not found: %s", id)
	}
	if item.Status == StatusPaused {
		m.mu.Unlock()
		return nil // already paused — idempotent
	}
	if item.Status != StatusDownloading && item.Status != StatusQueued && item.Status != StatusResolving {
		m.mu.Unlock()
		return fmt.Errorf("download not in pausable state: %s", item.Status)
	}
	if ok {
		cancel()
		delete(m.cancels, id)
	}
	item.Status = StatusPaused
	item.Speed = 0
	item.ETA = 0
	groupID := item.GroupID

	// Also pause all queued items in the same group so the user can choose which to resume
	var queuedToPause []*DownloadItem
	if groupID != "" {
		for _, other := range m.downloads {
			if other.ID != id && other.GroupID == groupID && other.Status == StatusQueued {
				queuedToPause = append(queuedToPause, other)
			}
		}
	}
	for _, other := range queuedToPause {
		if c, ok := m.cancels[other.ID]; ok {
			c()
			delete(m.cancels, other.ID)
		}
		other.Status = StatusPaused
		other.Speed = 0
		other.ETA = 0
	}
	m.mu.Unlock()

	m.emit(Event{Type: "paused", Data: *item})
	for _, other := range queuedToPause {
		m.emit(Event{Type: "paused", Data: *other})
	}
	m.saveHistory()
	return nil
}

func (m *Manager) ResumeDownload(id string) error {
	m.mu.Lock()
	item, exists := m.downloads[id]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("download not found: %s", id)
	}
	// Accept paused items, and queued items without a goroutine (orphaned from cascade)
	if item.Status != StatusPaused && !(item.Status == StatusQueued && m.cancels[id] == nil) {
		m.mu.Unlock()
		return fmt.Errorf("download not paused: %s", item.Status)
	}

	// Re-unrestrict if the URL might be expired (RD links expire)
	source := item.Source
	downloadURL := item.DownloadURL
	name := item.Name
	category := item.Category
	folder := item.Folder
	size := item.Size
	providerName := item.Provider

	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[id] = cancel
	item.Status = StatusQueued
	item.ScheduledFor = nil
	groupID := item.GroupID

	// Mark other paused items in the same group as queued (visual only — no goroutines)
	// They'll be picked up by autoResumeNext when slots free up
	if groupID != "" {
		for _, other := range m.downloads {
			if other.ID != id && other.GroupID == groupID && other.Status == StatusPaused &&
				other.DownloadURL != "" && other.Name != "" {
				other.Status = StatusQueued
				other.ScheduledFor = nil
			}
		}
	}
	m.mu.Unlock()

	m.emit(Event{Type: "resumed", Data: *item})

	go func() {
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			return // paused/cancelled while queued
		}
		defer func() { <-m.sem }()

		m.activeCount.Add(1)
		defer m.activeCount.Add(-1)

		m.updateStatus(item, StatusDownloading, "")

		// Try to re-unrestrict to get a fresh URL
		if strings.Contains(source, "real-debrid.com/d/") || !strings.HasPrefix(downloadURL, "http") {
			unrestricted, err := m.provider(providerName).UnrestrictLink(source)
			if err == nil {
				downloadURL = unrestricted.Download
				m.mu.Lock()
				item.DownloadURL = downloadURL
				m.mu.Unlock()
			}
		}

		destPath := filepath.Join(m.cfg.DownloadDir, name)
		numSegments := m.dynamicSegments()

		err := m.engine.Download(ctx, downloadURL, destPath, numSegments, func(downloaded, total, speed int64) {
			m.mu.Lock()
			item.Downloaded = downloaded
			if total > 0 {
				item.Size = total
				item.Progress = float64(downloaded) / float64(total)
			}
			item.Speed = speed
			if speed > 0 && total > 0 {
				item.ETA = (total - downloaded) / speed
			} else {
				item.ETA = 0
			}
			m.mu.Unlock()
			m.emit(Event{Type: "progress", Data: *item})
		}, m.statusCallback(item))

		if ctx.Err() != nil {
			return // paused or cancelled
		}

		if err != nil {
			m.setError(item, fmt.Sprintf("Download failed: %v", err))
			return
		}

		_ = size
		m.moveToFinal(ctx, item, destPath, category, folder)
	}()

	return nil
}

func (m *Manager) RetryMove(id string) error {
	m.mu.Lock()
	item, exists := m.downloads[id]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("download not found: %s", id)
	}
	if item.Status != StatusError {
		m.mu.Unlock()
		return fmt.Errorf("download not in error state: %s", item.Status)
	}
	// Check that the file exists in staging
	destPath := filepath.Join(m.cfg.DownloadDir, item.Name)
	if _, err := os.Stat(destPath); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("file not found in staging: %s", item.Name)
	}

	category := item.Category
	folder := item.Folder
	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[id] = cancel
	m.mu.Unlock()

	go m.moveToFinal(ctx, item, destPath, category, folder)
	return nil
}

func (m *Manager) RemoveDownload(id string) error {
	m.mu.Lock()
	if cancel, ok := m.cancels[id]; ok {
		cancel()
		delete(m.cancels, id)
	}
	item, exists := m.downloads[id]
	if !exists {
		m.mu.Unlock()
		return nil // already removed — idempotent
	}
	name := item.Name
	delete(m.downloads, id)
	m.mu.Unlock()
	m.saveHistory()

	// Clean up staging files
	m.cleanStagingFile(name)
	return nil
}

func (m *Manager) AddMagnet(magnet string, category Category, provider string) (*DownloadItem, error) {
	item := &DownloadItem{
		ID:        uuid.New().String()[:8],
		Name:      "Resolving magnet...",
		Category:  category,
		Status:    StatusPending,
		Source:    magnet,
		Provider:  provider,
		CreatedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.downloads[item.ID] = item
	m.cancels[item.ID] = cancel
	m.mu.Unlock()

	m.emit(Event{Type: "added", Data: *item})

	go m.processMagnet(ctx, item)
	return item, nil
}

func (m *Manager) AddRDBatch(links []string, groupName string, category Category, provider string) ([]*DownloadItem, error) {
	groupID := uuid.New().String()[:8]
	folder := groupName
	var items []*DownloadItem

	for _, link := range links {
		item, err := m.addRDLinkInternal(link, category, folder, groupID, groupName, provider)
		if err != nil {
			return items, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (m *Manager) AddRDLink(link string, category Category, folder string, provider string) (*DownloadItem, error) {
	return m.addRDLinkInternal(link, category, folder, "", "", provider)
}

func (m *Manager) addRDLinkInternal(link string, category Category, folder, groupID, groupName, provider string) (*DownloadItem, error) {
	item := &DownloadItem{
		ID:        uuid.New().String()[:8],
		Name:      "Resolving link...",
		Category:  category,
		Status:    StatusResolving,
		Source:    link,
		Provider:  provider,
		Folder:    folder,
		GroupID:   groupID,
		GroupName: groupName,
		CreatedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.downloads[item.ID] = item
	m.cancels[item.ID] = cancel
	m.mu.Unlock()

	m.emit(Event{Type: "added", Data: *item})

	go m.processRDLink(ctx, item)
	return item, nil
}

func (m *Manager) AddDirectURL(downloadURL, name string, category Category, provider string) (*DownloadItem, error) {
	item := &DownloadItem{
		ID:             uuid.New().String()[:8],
		Name:           name,
		Category:       category,
		Status:         StatusPending,
		Source:         downloadURL,
		Provider:       provider,
		DownloadURL:    downloadURL,
		SubtitleStatus: DetectSubtitleStatus(name),
		CreatedAt:      time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.downloads[item.ID] = item
	m.cancels[item.ID] = cancel
	m.mu.Unlock()

	m.emit(Event{Type: "added", Data: *item})

	go m.downloadFile(ctx, item)
	return item, nil
}

// AddMusicDownload queues a music file download from a direct URL.
// The file is downloaded to staging and then moved to MusicDir/folder/name.
func (m *Manager) AddMusicDownload(downloadURL, name, folder, groupID, groupName string) (*DownloadItem, error) {
	item := &DownloadItem{
		ID:          uuid.New().String()[:8],
		Name:        name,
		Category:    CategoryMusic,
		Status:      StatusPending,
		Source:      downloadURL,
		Provider:    "dab",
		Folder:      folder,
		GroupID:     groupID,
		GroupName:   groupName,
		DownloadURL: downloadURL,
		CreatedAt:   time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.downloads[item.ID] = item
	m.cancels[item.ID] = cancel
	m.mu.Unlock()

	m.emit(Event{Type: "added", Data: *item})

	go m.downloadFile(ctx, item)
	return item, nil
}

func (m *Manager) processMagnet(ctx context.Context, item *DownloadItem) {
	m.updateStatus(item, StatusQueued, "")
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-m.sem }()

	m.updateStatus(item, StatusResolving, "")

	resp, err := m.provider(item.Provider).AddMagnet(item.Source)
	if err != nil {
		m.setError(item, fmt.Sprintf("Failed to add magnet: %v", err))
		return
	}

	if err := m.provider(item.Provider).SelectFiles(resp.ID, "all"); err != nil {
		m.setError(item, fmt.Sprintf("Failed to select files: %v", err))
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}

		info, err := m.provider(item.Provider).GetTorrentInfo(resp.ID)
		if err != nil {
			m.setError(item, fmt.Sprintf("Failed to get torrent info: %v", err))
			return
		}

		m.mu.Lock()
		item.Name = info.Filename
		item.Size = info.Bytes
		item.Progress = info.Progress / 100.0
		if item.Category != CategoryMusic {
			item.Category = DetectCategory(info.Filename)
		}
		if info.Speed > 0 {
			item.Speed = info.Speed
		}
		m.mu.Unlock()
		m.emit(Event{Type: "progress", Data: *item})

		if info.Status == "downloaded" {
			category := item.Category
			if category != CategoryMusic {
				category = DetectCategory(info.Filename)
			}

			if len(info.Links) == 1 {
				// Single file — download directly using the original item
				unrestricted, err := m.provider(item.Provider).UnrestrictLink(info.Links[0])
				if err != nil {
					m.setError(item, fmt.Sprintf("Failed to unrestrict link: %v", err))
					_ = m.provider(item.Provider).DeleteTorrent(resp.ID)
					return
				}
				m.mu.Lock()
				item.DownloadURL = unrestricted.Download
				item.Name = unrestricted.Filename
				item.Size = unrestricted.Filesize
				item.Category = category
				item.SubtitleStatus = DetectSubtitleStatus(unrestricted.Filename)
				m.mu.Unlock()
				m.downloadFile(ctx, item)
			} else {
				// Multi-file torrent — create individual items per file (like batch downloads)
				groupID := uuid.New().String()[:8]
				folder := info.Filename

				// Update the original item as group parent so schedule can track via GroupID
				m.mu.Lock()
				item.GroupID = groupID
				item.GroupName = info.Filename
				item.Category = category
				item.Folder = folder
				m.mu.Unlock()

				// Release the semaphore slot so file downloads can use it
				<-m.sem

				for _, link := range info.Links {
					select {
					case <-ctx.Done():
						_ = m.provider(item.Provider).DeleteTorrent(resp.ID)
						return
					default:
					}

					_, err := m.addRDLinkInternal(link, category, folder, groupID, info.Filename, item.Provider)
					if err != nil {
						log.Printf("Failed to add file from magnet: %v", err)
						continue
					}
				}

				// Remove the resolver item now that individual items exist
				m.mu.Lock()
				delete(m.downloads, item.ID)
				delete(m.cancels, item.ID)
				m.mu.Unlock()
				m.emit(Event{Type: "removed", Data: *item})
				m.saveHistory()
			}

			_ = m.provider(item.Provider).DeleteTorrent(resp.ID)
			return
		}

		if info.Status == "error" || info.Status == "dead" || info.Status == "virus" {
			m.setError(item, fmt.Sprintf("Torrent %s", info.Status))
			return
		}
	}
}

func (m *Manager) processRDLink(ctx context.Context, item *DownloadItem) {
	// Unrestrict the link first (outside semaphore) so the name resolves immediately
	m.updateStatus(item, StatusResolving, "")
	unrestricted, err := m.provider(item.Provider).UnrestrictLink(item.Source)
	if err != nil {
		m.setError(item, fmt.Sprintf("Failed to resolve link: %v", err))
		return
	}

	m.mu.Lock()
	item.DownloadURL = unrestricted.Download
	item.Name = unrestricted.Filename
	item.Size = unrestricted.Filesize
	if item.Category != CategoryMusic {
		item.Category = DetectCategory(unrestricted.Filename)
	}
	item.SubtitleStatus = DetectSubtitleStatus(unrestricted.Filename)
	m.mu.Unlock()

	// Emit resolved name so the frontend updates before download starts
	m.emit(Event{Type: "resolved", Data: *item})

	// Now wait for a download slot
	m.updateStatus(item, StatusQueued, "")
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-m.sem }()

	m.downloadFileWithEngine(ctx, item)
}

func (m *Manager) statusCallback(item *DownloadItem) StatusCallback {
	return func(msg string) {
		m.mu.Lock()
		item.Error = msg
		m.mu.Unlock()
		m.emit(Event{Type: "progress", Data: *item})

		// Auto-clear "recovered" messages after 3 seconds
		if strings.Contains(msg, "recovered") {
			go func() {
				time.Sleep(3 * time.Second)
				m.mu.Lock()
				if item.Error == msg {
					item.Error = ""
				}
				m.mu.Unlock()
				m.emit(Event{Type: "progress", Data: *item})
			}()
		}
	}
}

func (m *Manager) dynamicSegments() int {
	active := int(m.activeCount.Load())
	if active < 1 {
		active = 1
	}
	n := 8 / active
	if n < 2 {
		n = 2
	}
	if n > m.cfg.MaxSegmentsPerFile {
		n = m.cfg.MaxSegmentsPerFile
	}
	return n
}

func (m *Manager) downloadFile(ctx context.Context, item *DownloadItem) {
	if item.DownloadURL == "" {
		m.setError(item, "No download URL")
		return
	}
	// Wait for a concurrency slot
	m.updateStatus(item, StatusQueued, "")
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-m.sem }()

	m.downloadFileWithEngine(ctx, item)
}

func (m *Manager) downloadFileWithEngine(ctx context.Context, item *DownloadItem) {
	if item.DownloadURL == "" {
		m.setError(item, "No download URL")
		return
	}

	m.activeCount.Add(1)
	defer m.activeCount.Add(-1)

	m.updateStatus(item, StatusDownloading, "")

	destPath := filepath.Join(m.cfg.DownloadDir, item.Name)
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		m.setError(item, fmt.Sprintf("Failed to create directory: %v", err))
		return
	}

	numSegments := m.dynamicSegments()
	// Music files use single segment — DAB/Qobuz stream URLs expire quickly
	// and the multi-segment engine can't refresh them.
	if item.Provider == "dab" {
		numSegments = 1
	}

	statusCb := m.statusCallback(item)
	err := m.engine.Download(ctx, item.DownloadURL, destPath, numSegments, func(downloaded, total, speed int64) {
		m.mu.Lock()
		item.Downloaded = downloaded
		if total > 0 {
			item.Size = total
			item.Progress = float64(downloaded) / float64(total)
		}
		item.Speed = speed
		if speed > 0 && total > 0 {
			item.ETA = (total - downloaded) / speed
		} else {
			item.ETA = 0
		}
		m.mu.Unlock()
		m.emit(Event{Type: "progress", Data: *item})
	}, statusCb)

	if ctx.Err() != nil {
		// Paused or cancelled — don't clean up the file
		return
	}

	// Whole-file retry with re-unrestrict (URL may have expired, or internet was down)
	// Retries: 30s, 1m, 2m, 5m — total ~8.5 min on top of the per-segment retries
	wholeFileBackoffs := []time.Duration{30 * time.Second, 1 * time.Minute, 2 * time.Minute, 5 * time.Minute}
	for retryNum := 0; err != nil && retryNum < len(wholeFileBackoffs); retryNum++ {
		if ctx.Err() != nil {
			return
		}

		log.Printf("Download failed (whole-file retry %d/%d): %s - %v",
			retryNum+1, len(wholeFileBackoffs), item.Name, err)

		m.mu.Lock()
		item.Error = fmt.Sprintf("Retrying in %s... (%d/%d)", wholeFileBackoffs[retryNum], retryNum+1, len(wholeFileBackoffs))
		m.mu.Unlock()
		m.emit(Event{Type: "progress", Data: *item})

		select {
		case <-time.After(wholeFileBackoffs[retryNum]):
		case <-ctx.Done():
			return
		}

		// Re-unrestrict to get a fresh download URL
		unrestricted, unreErr := m.provider(item.Provider).UnrestrictLink(item.Source)
		if unreErr != nil {
			log.Printf("Re-unrestrict failed: %v (will retry)", unreErr)
			err = fmt.Errorf("download: %v, re-unrestrict: %v", err, unreErr)
			continue
		}

		m.mu.Lock()
		item.DownloadURL = unrestricted.Download
		item.Error = ""
		m.mu.Unlock()

		err = m.engine.Download(ctx, unrestricted.Download, destPath, numSegments, func(downloaded, total, speed int64) {
			m.mu.Lock()
			item.Downloaded = downloaded
			if total > 0 {
				item.Size = total
				item.Progress = float64(downloaded) / float64(total)
			}
			item.Speed = speed
			if speed > 0 && total > 0 {
				item.ETA = (total - downloaded) / speed
			} else {
				item.ETA = 0
			}
			m.mu.Unlock()
			m.emit(Event{Type: "progress", Data: *item})
		}, statusCb)
	}

	if ctx.Err() != nil {
		return
	}
	if err != nil {
		m.setError(item, fmt.Sprintf("Download failed after all retries: %v", err))
		return
	}

	m.moveToFinal(ctx, item, destPath, item.Category, item.Folder)
}

func (m *Manager) moveToFinal(ctx context.Context, item *DownloadItem, destPath string, category Category, folder string) {
	m.updateStatus(item, StatusMoving, "")

	var finalDir string
	switch category {
	case CategoryMovies:
		finalDir = m.cfg.MoviesDir
	case CategoryTVShows:
		finalDir = m.cfg.TVShowsDir
	case CategoryMusic:
		finalDir = m.cfg.MusicDir
	default:
		finalDir = m.cfg.MoviesDir
	}

	if folder != "" {
		finalDir = filepath.Join(finalDir, folder)
	}

	finalPath := filepath.Join(finalDir, item.Name)

	// Remove existing destination file if present (may be corrupt from interrupted move)
	if fileExists(finalPath) {
		log.Printf("Destination file already exists, overwriting: %s", finalPath)
		os.Remove(finalPath)
	}

	// Retry move with backoffs if destination is unavailable (e.g. external drive offline)
	moveBackoffs := []time.Duration{10 * time.Second, 30 * time.Second, 1 * time.Minute, 2 * time.Minute, 5 * time.Minute}
	var moveErr error
	for attempt := 0; attempt <= len(moveBackoffs); attempt++ {
		if ctx.Err() != nil {
			return
		}

		if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
			moveErr = fmt.Errorf("create destination: %v", err)
			if attempt < len(moveBackoffs) {
				m.mu.Lock()
				item.Error = fmt.Sprintf("Drive unavailable, retrying in %s... (%d/%d)", moveBackoffs[attempt], attempt+1, len(moveBackoffs))
				m.mu.Unlock()
				m.emit(Event{Type: "progress", Data: *item})
				select {
				case <-time.After(moveBackoffs[attempt]):
				case <-ctx.Done():
					return
				}
			}
			continue
		}

		// Try rename first (same filesystem), fallback to buffered copy with progress
		if err := os.Rename(destPath, finalPath); err != nil {
			if err := CopyFileBuffered(ctx, destPath, finalPath, func(copied, total int64) {
				m.mu.Lock()
				if total > 0 {
					item.Progress = float64(copied) / float64(total)
				}
				m.mu.Unlock()
				m.emit(Event{Type: "progress", Data: *item})
			}); err != nil {
				moveErr = fmt.Errorf("move file: %v", err)
				if attempt < len(moveBackoffs) {
					m.mu.Lock()
					item.Error = fmt.Sprintf("Move failed, retrying in %s... (%d/%d)", moveBackoffs[attempt], attempt+1, len(moveBackoffs))
					m.mu.Unlock()
					m.emit(Event{Type: "progress", Data: *item})
					select {
					case <-time.After(moveBackoffs[attempt]):
					case <-ctx.Done():
						return
					}
				}
				continue
			}
			os.Remove(destPath)
		}

		moveErr = nil
		break
	}

	if moveErr != nil {
		m.setError(item, fmt.Sprintf("Failed to move file (drive offline?): %v — file saved in staging", moveErr))
		return
	}

	// Probe for actual embedded subtitles post-download (skip for music)
	subStatus := SubtitleNone
	if category != CategoryMusic {
		hasSubs, _ := media.ProbeSubtitles(finalPath)
		if hasSubs {
			subStatus = SubtitleConfirmed
		}
	}

	now := time.Now()
	m.mu.Lock()
	item.Status = StatusCompleted
	item.Progress = 1.0
	item.FilePath = finalPath
	item.CompletedAt = &now
	item.Speed = 0
	item.ETA = 0
	item.SubtitleStatus = subStatus
	m.markCompleted(item.Source)
	m.mu.Unlock()

	m.emit(Event{Type: "completed", Data: *item})
	m.saveHistory()
	log.Printf("Download complete: %s -> %s", item.Name, finalPath)

	// Post-move hook (e.g. music metadata tagging)
	if m.postMoveHook != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("postMoveHook panic for %s: %v", item.Name, r)
				}
			}()
			m.postMoveHook(item.ID, finalPath)
		}()
	}

	// Auto-resume next paused download in the same group
	m.autoResumeNext(item.GroupID)
}

// autoResumeNext finds the next paused download and resumes it.
// Prioritizes groups/singles that have zero active downloads (fairness),
// then falls back to same group, then any paused item.
func (m *Manager) autoResumeNext(groupID string) {
	m.mu.RLock()

	// Count active downloads per group/single
	activePerGroup := make(map[string]int)
	for _, item := range m.downloads {
		if item.Status == StatusDownloading || item.Status == StatusQueued || item.Status == StatusResolving || item.Status == StatusMoving {
			key := item.GroupID
			if key == "" {
				key = item.ID // singles use their own ID
			}
			activePerGroup[key]++
		}
	}

	// Find paused/queued candidates, preferring groups with zero active (starved)
	// Queued items without a cancel func are "orphaned" from cascade and need a goroutine
	var sameGroup, starved, fallback *DownloadItem
	for _, item := range m.downloads {
		isCandidate := (item.Status == StatusPaused || (item.Status == StatusQueued && m.cancels[item.ID] == nil)) &&
			item.DownloadURL != "" && item.Name != "" && item.ScheduledFor == nil
		if !isCandidate {
			continue
		}
		key := item.GroupID
		if key == "" {
			key = item.ID
		}

		if activePerGroup[key] == 0 {
			// This group/single has nothing active — prioritize it
			if starved == nil || strings.ToLower(item.Name) < strings.ToLower(starved.Name) {
				starved = item
			}
		} else if groupID != "" && item.GroupID == groupID {
			// Same group as the one that just freed a slot
			if sameGroup == nil || strings.ToLower(item.Name) < strings.ToLower(sameGroup.Name) {
				sameGroup = item
			}
		} else {
			if fallback == nil || strings.ToLower(item.Name) < strings.ToLower(fallback.Name) {
				fallback = item
			}
		}
	}
	m.mu.RUnlock()

	// Pick: starved group first, then same group, then any
	candidate := starved
	if candidate == nil {
		candidate = sameGroup
	}
	if candidate == nil {
		candidate = fallback
	}

	if candidate != nil {
		log.Printf("Auto-resuming next download: %s (%s)", candidate.ID, candidate.Name)
		_ = m.ResumeDownload(candidate.ID)
	}
}

// setScheduledFor marks downloads as scheduled for a specific time.
func (m *Manager) setScheduledFor(ids []string, t *time.Time) {
	m.mu.Lock()
	for _, id := range ids {
		if item, ok := m.downloads[id]; ok {
			item.ScheduledFor = t
			m.emit(Event{Type: "progress", Data: *item})
		}
	}
	m.mu.Unlock()
	m.saveHistory()
}

func (m *Manager) updateStatus(item *DownloadItem, status Status, errMsg string) {
	m.mu.Lock()
	item.Status = status
	if errMsg != "" {
		item.Error = errMsg
	}
	m.mu.Unlock()
	m.emit(Event{Type: "progress", Data: *item})
}

func (m *Manager) setError(item *DownloadItem, errMsg string) {
	m.mu.Lock()
	item.Status = StatusError
	item.Error = errMsg
	item.Speed = 0
	groupID := item.GroupID
	m.mu.Unlock()
	m.emit(Event{Type: "error", Data: *item})
	m.saveHistory()
	log.Printf("Download error [%s]: %s", item.ID, errMsg)

	// Auto-resume next paused download
	m.autoResumeNext(groupID)
}

func (m *Manager) saveHistory() {
	m.mu.RLock()
	items := make([]DownloadItem, 0, len(m.downloads))
	for _, item := range m.downloads {
		items = append(items, *item)
	}
	m.mu.RUnlock()

	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.historyFile), 0755)
	_ = os.WriteFile(m.historyFile, data, 0644)
}

func (m *Manager) loadHistory() {
	data, err := os.ReadFile(m.historyFile)
	if err != nil {
		return
	}
	var items []DownloadItem
	if err := json.Unmarshal(data, &items); err != nil {
		return
	}

	var toResume []*DownloadItem
	var toMove []*DownloadItem
	for i := range items {
		item := items[i]
		// Default empty Provider to "realdebrid" for backward compatibility
		if item.Provider == "" {
			item.Provider = "realdebrid"
		}
		if item.Status == StatusMoving {
			// Was moving when interrupted — check if file exists in staging
			stagingPath := filepath.Join(m.cfg.DownloadDir, item.Name)
			if item.Name != "" && fileExists(stagingPath) {
				item.Speed = 0
				item.ETA = 0
				item.Error = ""
				m.downloads[item.ID] = &item
				toMove = append(toMove, m.downloads[item.ID])
				continue
			}
			// File gone from staging — mark as error
			item.Status = StatusError
			item.Error = "interrupted by restart, staging file missing"
			item.Speed = 0
			m.downloads[item.ID] = &item
			continue
		}
		if item.Status == StatusDownloading {
			// Has a download URL and a .part file may exist — auto-resume
			if item.DownloadURL != "" && item.Name != "" {
				item.Status = StatusPaused
				item.Speed = 0
				item.ETA = 0
				item.Error = ""
				m.downloads[item.ID] = &item
				// Don't auto-resume if scheduled for later
				if item.ScheduledFor == nil {
					toResume = append(toResume, m.downloads[item.ID])
				}
				continue
			}
			// No URL — can't resume, mark as error
			item.Status = StatusError
			item.Error = "interrupted by restart"
			item.Speed = 0
		}
		if item.Status == StatusResolving || item.Status == StatusPending || item.Status == StatusQueued {
			// Resolving/pending/queued can't be resumed (no download URL yet)
			item.Status = StatusError
			item.Error = "interrupted by restart"
			item.Speed = 0
		}
		// Keep paused downloads as-is — they were intentionally paused by the user
		// Only downloads that were StatusDownloading at crash time get auto-resumed (handled above)
		m.downloads[item.ID] = &item
	}

	// Auto-resume interrupted moves after a short delay
	if len(toMove) > 0 {
		go func() {
			time.Sleep(2 * time.Second)
			for _, item := range toMove {
				log.Printf("Auto-resuming interrupted move: %s (%s)", item.ID, item.Name)
				ctx := context.Background()
				destPath := filepath.Join(m.cfg.DownloadDir, item.Name)
				m.sem <- struct{}{}
				m.activeCount.Add(1)
				go func(item *DownloadItem, destPath string) {
					defer func() { <-m.sem; m.activeCount.Add(-1) }()
					m.moveToFinal(ctx, item, destPath, item.Category, item.Folder)
				}(item, destPath)
			}
		}()
	}

	// Auto-resume interrupted downloads after a short delay (let the server fully start)
	if len(toResume) > 0 {
		go func() {
			time.Sleep(2 * time.Second)
			for _, item := range toResume {
				log.Printf("Auto-resuming interrupted download: %s (%s)", item.ID, item.Name)
				if err := m.ResumeDownload(item.ID); err != nil {
					log.Printf("Auto-resume failed for %s: %v", item.ID, err)
				}
			}
		}()
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

func (m *Manager) loadCompletedSources() {
	data, err := os.ReadFile(m.completedFile)
	if err != nil {
		return
	}
	var sources []string
	if json.Unmarshal(data, &sources) == nil {
		for _, s := range sources {
			m.completedSources[s] = true
		}
	}
}

func (m *Manager) saveCompletedSources() {
	m.mu.RLock()
	sources := make([]string, 0, len(m.completedSources))
	for s := range m.completedSources {
		sources = append(sources, s)
	}
	m.mu.RUnlock()
	data, err := json.Marshal(sources)
	if err != nil {
		return
	}
	_ = os.WriteFile(m.completedFile, data, 0644)
}

func (m *Manager) markCompleted(source string) {
	m.completedSources[source] = true
	go m.saveCompletedSources()
}

// GetCompletedSources returns all sources that have been successfully downloaded.
func (m *Manager) GetCompletedSources() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sources := make([]string, 0, len(m.completedSources))
	for s := range m.completedSources {
		sources = append(sources, s)
	}
	return sources
}

// cleanStagingFile removes a specific file and its .part file from the staging directory.
func (m *Manager) cleanStagingFile(name string) {
	if name == "" {
		return
	}
	staging := filepath.Join(m.cfg.DownloadDir, name)
	partFile := staging + ".part"
	if err := os.Remove(staging); err == nil {
		log.Printf("Cleaned staging file: %s", name)
	}
	if err := os.Remove(partFile); err == nil {
		log.Printf("Cleaned part file: %s.part", name)
	}
}

// cleanStaleStagingFiles removes staging files and .part files that don't belong
// to any active or paused download.
func (m *Manager) cleanStaleStagingFiles() {
	entries, err := os.ReadDir(m.cfg.DownloadDir)
	if err != nil {
		return
	}

	// Build set of expected filenames from active/paused downloads
	m.mu.RLock()
	expected := make(map[string]bool)
	for _, item := range m.downloads {
		if item.Status == StatusDownloading || item.Status == StatusPaused ||
			item.Status == StatusMoving || item.Status == StatusResolving ||
			item.Status == StatusPending || item.Status == StatusQueued {
			if item.Name != "" {
				expected[item.Name] = true
				expected[item.Name+".part"] = true
			}
		}
	}
	m.mu.RUnlock()

	for _, entry := range entries {
		name := entry.Name()
		// Skip history and schedule files
		if name == ".history.json" || name == ".schedules.json" {
			continue
		}
		// Skip directories
		if entry.IsDir() {
			continue
		}
		// Skip files belonging to active downloads
		if expected[name] {
			continue
		}
		// Only clean .part files from orphaned downloads
		if filepath.Ext(name) == ".part" || filepath.Ext(name) == ".tmp" {
			path := filepath.Join(m.cfg.DownloadDir, name)
			info, err := entry.Info()
			if err != nil {
				continue
			}
			// Only remove if older than 1 hour (safety margin)
			if time.Since(info.ModTime()) > 1*time.Hour {
				log.Printf("Cleaning stale file: %s", name)
				os.Remove(path)
			}
		}
	}
}

// StartCleanup begins periodic cleanup of stale staging files.
func (m *Manager) StartCleanup(stopCh <-chan struct{}) {
	// Initial cleanup on startup (delayed)
	go func() {
		time.Sleep(30 * time.Second)
		m.cleanStaleStagingFiles()

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				m.cleanStaleStagingFiles()
			}
		}
	}()
}
