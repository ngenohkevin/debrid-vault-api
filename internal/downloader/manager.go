package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ngenohkevin/debrid-vault-api/internal/config"
	"github.com/ngenohkevin/debrid-vault-api/internal/realdebrid"
)

type Manager struct {
	cfg         *config.Config
	rdClient    *realdebrid.Client
	engine      *DownloadEngine
	downloads   map[string]*DownloadItem
	cancels     map[string]context.CancelFunc
	mu          sync.RWMutex
	subs        map[chan Event]struct{}
	subsMu      sync.RWMutex
	sem         chan struct{}
	activeCount atomic.Int32
	historyFile string
}

func NewManager(cfg *config.Config, rdClient *realdebrid.Client) *Manager {
	m := &Manager{
		cfg:         cfg,
		rdClient:    rdClient,
		engine:      NewDownloadEngine(cfg.MaxSegmentsPerFile, cfg.SpeedLimitMbps),
		downloads:   make(map[string]*DownloadItem),
		cancels:     make(map[string]context.CancelFunc),
		subs:        make(map[chan Event]struct{}),
		sem:         make(chan struct{}, cfg.MaxConcurrentDownloads),
		historyFile: filepath.Join(cfg.DownloadDir, ".history.json"),
	}
	m.loadHistory()
	return m
}

func (m *Manager) Engine() *DownloadEngine {
	return m.engine
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	for id, cancel := range m.cancels {
		cancel()
		if item, ok := m.downloads[id]; ok {
			if item.Status == StatusDownloading || item.Status == StatusResolving || item.Status == StatusMoving {
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
	if item.Status == StatusDownloading || item.Status == StatusResolving || item.Status == StatusPending {
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
	if item.Status != StatusDownloading {
		m.mu.Unlock()
		return fmt.Errorf("download not in downloading state: %s", item.Status)
	}
	if ok {
		cancel()
		delete(m.cancels, id)
	}
	item.Status = StatusPaused
	item.Speed = 0
	item.ETA = 0
	m.mu.Unlock()

	m.emit(Event{Type: "paused", Data: *item})
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
	if item.Status != StatusPaused {
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

	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[id] = cancel
	item.Status = StatusDownloading
	m.mu.Unlock()

	m.emit(Event{Type: "resumed", Data: *item})

	go func() {
		m.sem <- struct{}{}
		defer func() { <-m.sem }()
		m.activeCount.Add(1)
		defer m.activeCount.Add(-1)

		// Try to re-unrestrict to get a fresh URL
		if strings.Contains(source, "real-debrid.com/d/") || !strings.HasPrefix(downloadURL, "http") {
			unrestricted, err := m.rdClient.UnrestrictLink(source)
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
		return fmt.Errorf("download not found: %s", id)
	}
	name := item.Name
	delete(m.downloads, id)
	m.mu.Unlock()
	m.saveHistory()

	// Clean up staging files
	m.cleanStagingFile(name)
	return nil
}

func (m *Manager) AddMagnet(magnet string, category Category) (*DownloadItem, error) {
	item := &DownloadItem{
		ID:        uuid.New().String()[:8],
		Name:      "Resolving magnet...",
		Category:  category,
		Status:    StatusPending,
		Source:    magnet,
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

func (m *Manager) AddRDBatch(links []string, groupName string, category Category) ([]*DownloadItem, error) {
	groupID := uuid.New().String()[:8]
	folder := groupName
	var items []*DownloadItem

	for _, link := range links {
		item, err := m.addRDLinkInternal(link, category, folder, groupID, groupName)
		if err != nil {
			return items, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (m *Manager) AddRDLink(link string, category Category, folder string) (*DownloadItem, error) {
	return m.addRDLinkInternal(link, category, folder, "", "")
}

func (m *Manager) addRDLinkInternal(link string, category Category, folder, groupID, groupName string) (*DownloadItem, error) {
	item := &DownloadItem{
		ID:        uuid.New().String()[:8],
		Name:      "Resolving RD link...",
		Category:  category,
		Status:    StatusResolving,
		Source:    link,
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

func (m *Manager) AddDirectURL(downloadURL, name string, category Category) (*DownloadItem, error) {
	item := &DownloadItem{
		ID:          uuid.New().String()[:8],
		Name:        name,
		Category:    category,
		Status:      StatusPending,
		Source:      downloadURL,
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
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	m.updateStatus(item, StatusResolving, "")

	resp, err := m.rdClient.AddMagnet(item.Source)
	if err != nil {
		m.setError(item, fmt.Sprintf("Failed to add magnet: %v", err))
		return
	}

	if err := m.rdClient.SelectFiles(resp.ID, "all"); err != nil {
		m.setError(item, fmt.Sprintf("Failed to select files: %v", err))
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}

		info, err := m.rdClient.GetTorrentInfo(resp.ID)
		if err != nil {
			m.setError(item, fmt.Sprintf("Failed to get torrent info: %v", err))
			return
		}

		m.mu.Lock()
		item.Name = info.Filename
		item.Size = info.Bytes
		item.Progress = info.Progress / 100.0
		item.Category = DetectCategory(info.Filename)
		if info.Speed > 0 {
			item.Speed = info.Speed
		}
		m.mu.Unlock()
		m.emit(Event{Type: "progress", Data: *item})

		if info.Status == "downloaded" {
			for _, link := range info.Links {
				select {
				case <-ctx.Done():
					return
				default:
				}

				unrestricted, err := m.rdClient.UnrestrictLink(link)
				if err != nil {
					log.Printf("Failed to unrestrict link: %v", err)
					continue
				}

				m.mu.Lock()
				item.DownloadURL = unrestricted.Download
				item.Name = unrestricted.Filename
				item.Size = unrestricted.Filesize
				m.mu.Unlock()

				m.downloadFile(ctx, item)
			}

			_ = m.rdClient.DeleteTorrent(resp.ID)
			return
		}

		if info.Status == "error" || info.Status == "dead" || info.Status == "virus" {
			m.setError(item, fmt.Sprintf("Torrent %s", info.Status))
			return
		}
	}
}

func (m *Manager) processRDLink(ctx context.Context, item *DownloadItem) {
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	// Unrestrict the link directly (no ListDownloads scan)
	unrestricted, err := m.rdClient.UnrestrictLink(item.Source)
	if err != nil {
		m.setError(item, fmt.Sprintf("Failed to resolve link: %v", err))
		return
	}

	m.mu.Lock()
	item.DownloadURL = unrestricted.Download
	item.Name = unrestricted.Filename
	item.Size = unrestricted.Filesize
	// Auto-correct category based on actual filename
	item.Category = DetectCategory(unrestricted.Filename)
	m.mu.Unlock()

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
		unrestricted, unreErr := m.rdClient.UnrestrictLink(item.Source)
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
	default:
		finalDir = m.cfg.MoviesDir
	}

	if folder != "" {
		finalDir = filepath.Join(finalDir, folder)
	}

	finalPath := filepath.Join(finalDir, item.Name)

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

	now := time.Now()
	m.mu.Lock()
	item.Status = StatusCompleted
	item.Progress = 1.0
	item.FilePath = finalPath
	item.CompletedAt = &now
	item.Speed = 0
	item.ETA = 0
	m.mu.Unlock()

	m.emit(Event{Type: "completed", Data: *item})
	m.saveHistory()
	log.Printf("Download complete: %s -> %s", item.Name, finalPath)
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
	m.mu.Unlock()
	m.emit(Event{Type: "error", Data: *item})
	m.saveHistory()
	log.Printf("Download error [%s]: %s", item.ID, errMsg)
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
	for i := range items {
		item := items[i]
		if item.Status == StatusDownloading || item.Status == StatusMoving {
			// Has a download URL and a .part file may exist — auto-resume
			if item.DownloadURL != "" && item.Name != "" {
				item.Status = StatusPaused
				item.Speed = 0
				item.ETA = 0
				item.Error = ""
				m.downloads[item.ID] = &item
				toResume = append(toResume, m.downloads[item.ID])
				continue
			}
			// No URL — can't resume, mark as error
			item.Status = StatusError
			item.Error = "interrupted by restart"
			item.Speed = 0
		}
		if item.Status == StatusResolving || item.Status == StatusPending {
			// Resolving/pending can't be resumed (no download URL yet)
			item.Status = StatusError
			item.Error = "interrupted by restart"
			item.Speed = 0
		}
		// Auto-resume paused downloads that were interrupted by shutdown
		if item.Status == StatusPaused && item.DownloadURL != "" && item.Name != "" {
			m.downloads[item.ID] = &item
			toResume = append(toResume, m.downloads[item.ID])
			continue
		}
		m.downloads[item.ID] = &item
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
			item.Status == StatusPending {
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
