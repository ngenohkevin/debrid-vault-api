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
	"time"

	"github.com/google/uuid"
	"github.com/ngenohkevin/debrid-vault-api/internal/config"
	"github.com/ngenohkevin/debrid-vault-api/internal/realdebrid"
)

type Manager struct {
	cfg         *config.Config
	rdClient    *realdebrid.Client
	downloads   map[string]*DownloadItem
	cancels     map[string]context.CancelFunc
	mu          sync.RWMutex
	subs        map[chan Event]struct{}
	subsMu      sync.RWMutex
	sem         chan struct{}
	historyFile string
}

func NewManager(cfg *config.Config, rdClient *realdebrid.Client) *Manager {
	m := &Manager{
		cfg:         cfg,
		rdClient:    rdClient,
		downloads:   make(map[string]*DownloadItem),
		cancels:     make(map[string]context.CancelFunc),
		subs:        make(map[chan Event]struct{}),
		sem:         make(chan struct{}, 2), // max 2 concurrent downloads
		historyFile: filepath.Join(cfg.DownloadDir, ".history.json"),
	}
	m.loadHistory()
	return m
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	for id, cancel := range m.cancels {
		cancel()
		if item, ok := m.downloads[id]; ok {
			if item.Status == StatusDownloading || item.Status == StatusResolving {
				item.Status = StatusCancelled
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
			// drop if subscriber is slow
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
	m.mu.Unlock()
	m.emit(Event{Type: "cancelled", Data: *item})
	m.saveHistory()
	return nil
}

func (m *Manager) RemoveDownload(id string) error {
	m.mu.Lock()
	if cancel, ok := m.cancels[id]; ok {
		cancel()
		delete(m.cancels, id)
	}
	_, exists := m.downloads[id]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("download not found: %s", id)
	}
	delete(m.downloads, id)
	m.mu.Unlock()
	m.saveHistory()
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

func (m *Manager) AddRDLink(link string, category Category) (*DownloadItem, error) {
	item := &DownloadItem{
		ID:        uuid.New().String()[:8],
		Name:      "Resolving RD link...",
		Category:  category,
		Status:    StatusResolving,
		Source:    link,
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

	// Add magnet to RD
	resp, err := m.rdClient.AddMagnet(item.Source)
	if err != nil {
		m.setError(item, fmt.Sprintf("Failed to add magnet: %v", err))
		return
	}

	// Select all files
	if err := m.rdClient.SelectFiles(resp.ID, "all"); err != nil {
		m.setError(item, fmt.Sprintf("Failed to select files: %v", err))
		return
	}

	// Poll until torrent is ready
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
		if info.Speed > 0 {
			item.Speed = info.Speed
		}
		m.mu.Unlock()
		m.emit(Event{Type: "progress", Data: *item})

		if info.Status == "downloaded" {
			// Unrestrict links and download
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

			// Cleanup torrent from RD
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

	// Try to find the download in RD cloud
	downloads, err := m.rdClient.ListDownloads(100)
	if err != nil {
		m.setError(item, fmt.Sprintf("Failed to list RD downloads: %v", err))
		return
	}

	var downloadURL string
	var filename string
	var filesize int64

	for _, dl := range downloads {
		if dl.Link == item.Source || strings.Contains(item.Source, dl.ID) {
			downloadURL = dl.Download
			filename = dl.Filename
			filesize = dl.Filesize
			break
		}
	}

	if downloadURL == "" {
		// Try unrestricting the link directly
		unrestricted, err := m.rdClient.UnrestrictLink(item.Source)
		if err != nil {
			m.setError(item, fmt.Sprintf("Failed to resolve link: %v", err))
			return
		}
		downloadURL = unrestricted.Download
		filename = unrestricted.Filename
		filesize = unrestricted.Filesize
	}

	m.mu.Lock()
	item.DownloadURL = downloadURL
	item.Name = filename
	item.Size = filesize
	m.mu.Unlock()

	m.downloadFile(ctx, item)
}

func (m *Manager) downloadFile(ctx context.Context, item *DownloadItem) {
	if item.DownloadURL == "" {
		m.setError(item, "No download URL")
		return
	}

	m.updateStatus(item, StatusDownloading, "")

	// Create staging file
	destPath := filepath.Join(m.cfg.DownloadDir, item.Name)
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		m.setError(item, fmt.Sprintf("Failed to create directory: %v", err))
		return
	}

	file, err := os.Create(destPath)
	if err != nil {
		m.setError(item, fmt.Sprintf("Failed to create file: %v", err))
		return
	}
	defer file.Close()

	var lastEmit time.Time
	var lastBytes int64
	var lastTime time.Time

	err = m.rdClient.DownloadFile(item.DownloadURL, file, func(downloaded, total int64) {
		select {
		case <-ctx.Done():
			return
		default:
		}

		now := time.Now()
		if now.Sub(lastEmit) < 500*time.Millisecond {
			return
		}

		// Calculate speed
		elapsed := now.Sub(lastTime).Seconds()
		var speed int64
		if elapsed > 0 && lastTime != (time.Time{}) {
			speed = int64(float64(downloaded-lastBytes) / elapsed)
		}

		// Calculate ETA
		var eta int64
		if speed > 0 && total > 0 {
			eta = (total - downloaded) / speed
		}

		m.mu.Lock()
		item.Downloaded = downloaded
		item.Size = total
		if total > 0 {
			item.Progress = float64(downloaded) / float64(total)
		}
		item.Speed = speed
		item.ETA = eta
		m.mu.Unlock()

		m.emit(Event{Type: "progress", Data: *item})
		lastEmit = now
		lastBytes = downloaded
		lastTime = now
	})

	if ctx.Err() != nil {
		os.Remove(destPath)
		return
	}

	// Retry on network errors (up to 3 attempts)
	for attempt := 1; err != nil && attempt <= 3; attempt++ {
		if ctx.Err() != nil {
			os.Remove(destPath)
			return
		}
		log.Printf("Download failed (attempt %d/3): %s - %v. Retrying in 10s...", attempt, item.Name, err)
		m.mu.Lock()
		item.Error = fmt.Sprintf("Retry %d/3 in 10s...", attempt)
		m.mu.Unlock()
		m.emit(Event{Type: "progress", Data: *item})

		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
			os.Remove(destPath)
			return
		}

		// Re-create file for retry
		file.Close()
		os.Remove(destPath)
		file, err = os.Create(destPath)
		if err != nil {
			m.setError(item, fmt.Sprintf("Failed to create file: %v", err))
			return
		}

		m.mu.Lock()
		item.Error = ""
		item.Downloaded = 0
		item.Progress = 0
		m.mu.Unlock()
		m.updateStatus(item, StatusDownloading, "")
		lastEmit = time.Time{}
		lastBytes = 0
		lastTime = time.Time{}

		err = m.rdClient.DownloadFile(item.DownloadURL, file, func(downloaded, total int64) {
			select {
			case <-ctx.Done():
				return
			default:
			}
			now := time.Now()
			if now.Sub(lastEmit) < 500*time.Millisecond {
				return
			}
			elapsed := now.Sub(lastTime).Seconds()
			var speed int64
			if elapsed > 0 && lastTime != (time.Time{}) {
				speed = int64(float64(downloaded-lastBytes) / elapsed)
			}
			var eta int64
			if speed > 0 && total > 0 {
				eta = (total - downloaded) / speed
			}
			m.mu.Lock()
			item.Downloaded = downloaded
			item.Size = total
			if total > 0 {
				item.Progress = float64(downloaded) / float64(total)
			}
			item.Speed = speed
			item.ETA = eta
			m.mu.Unlock()
			m.emit(Event{Type: "progress", Data: *item})
			lastEmit = now
			lastBytes = downloaded
			lastTime = now
		})
	}

	if err != nil {
		os.Remove(destPath)
		m.setError(item, fmt.Sprintf("Download failed after 3 attempts: %v", err))
		return
	}

	// Move to destination
	m.updateStatus(item, StatusMoving, "")

	var finalDir string
	switch item.Category {
	case CategoryMovies:
		finalDir = m.cfg.MoviesDir
	case CategoryTVShows:
		finalDir = m.cfg.TVShowsDir
	default:
		finalDir = m.cfg.MoviesDir
	}

	finalPath := filepath.Join(finalDir, item.Name)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
		m.setError(item, fmt.Sprintf("Failed to create destination: %v", err))
		return
	}

	// Try rename first (same filesystem), fallback to copy
	if err := os.Rename(destPath, finalPath); err != nil {
		if err := copyFile(destPath, finalPath); err != nil {
			m.setError(item, fmt.Sprintf("Failed to move file: %v", err))
			return
		}
		os.Remove(destPath)
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
	for i := range items {
		item := items[i]
		// Reset any stuck downloads
		if item.Status == StatusDownloading || item.Status == StatusResolving || item.Status == StatusMoving {
			item.Status = StatusError
			item.Error = "interrupted by restart"
			item.Speed = 0
		}
		m.downloads[item.ID] = &item
	}
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

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}
	return out.Close()
}
