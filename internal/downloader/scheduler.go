package downloader

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type ScheduleStatus string

const (
	ScheduleStatusScheduled ScheduleStatus = "scheduled"
	ScheduleStatusRunning   ScheduleStatus = "running"
	ScheduleStatusCompleted ScheduleStatus = "completed"
	ScheduleStatusCancelled ScheduleStatus = "cancelled"
	ScheduleStatusError     ScheduleStatus = "error"
)

type ScheduledDownload struct {
	ID             string         `json:"id"`
	Name           string         `json:"name,omitempty"`
	Source         string         `json:"source"`
	Category       Category       `json:"category"`
	Folder         string         `json:"folder,omitempty"`
	ScheduledAt    time.Time      `json:"scheduledAt"`
	SpeedLimitMbps float64        `json:"speedLimitMbps"`
	Status         ScheduleStatus `json:"status"`
	Error          string         `json:"error,omitempty"`
	DownloadID     string         `json:"downloadId,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
	CompletedAt    *time.Time     `json:"completedAt,omitempty"`
}

type Scheduler struct {
	manager      *Manager
	schedules    map[string]*ScheduledDownload
	mu           sync.RWMutex
	scheduleFile string
	stopCh       chan struct{}
	prevLimit    float64
	prevLimitMu  sync.Mutex
}

func NewScheduler(manager *Manager) *Scheduler {
	s := &Scheduler{
		manager:      manager,
		schedules:    make(map[string]*ScheduledDownload),
		scheduleFile: filepath.Join(manager.cfg.DownloadDir, ".schedules.json"),
		stopCh:       make(chan struct{}),
	}
	s.loadSchedules()
	s.retryInterrupted()
	go s.run()
	return s
}

func (s *Scheduler) Stop() {
	close(s.stopCh)
}

func (s *Scheduler) AddSchedule(name, source string, category Category, folder string, scheduledAt time.Time, speedLimitMbps float64) *ScheduledDownload {
	// Extract name from source if not provided
	if name == "" {
		if strings.HasPrefix(source, "magnet:") {
			if u, err := url.Parse(source); err == nil {
				if dn := u.Query().Get("dn"); dn != "" {
					name = dn
				}
			}
		} else if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
			if u, err := url.Parse(source); err == nil {
				base := path.Base(u.Path)
				if base != "" && base != "." && base != "/" {
					name = base
				}
			}
		}
	}
	sched := &ScheduledDownload{
		ID:             uuid.New().String()[:8],
		Name:           name,
		Source:         source,
		Category:       category,
		Folder:         folder,
		ScheduledAt:    scheduledAt,
		SpeedLimitMbps: speedLimitMbps,
		Status:         ScheduleStatusScheduled,
		CreatedAt:      time.Now(),
	}

	s.mu.Lock()
	s.schedules[sched.ID] = sched
	s.mu.Unlock()

	s.saveSchedules()
	log.Printf("Schedule added: %s at %s (limit: %.0f Mbps)", sched.ID, scheduledAt.Format(time.RFC3339), speedLimitMbps)
	return sched
}

func (s *Scheduler) UpdateSchedule(id string, scheduledAt *time.Time, speedLimitMbps *float64) (*ScheduledDownload, error) {
	s.mu.Lock()
	sched, ok := s.schedules[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("schedule not found: %s", id)
	}
	// Allow editing scheduled or errored schedules (reschedule)
	if sched.Status != ScheduleStatusScheduled && sched.Status != ScheduleStatusError {
		s.mu.Unlock()
		return nil, fmt.Errorf("cannot edit schedule in %s state", sched.Status)
	}
	if scheduledAt != nil {
		sched.ScheduledAt = *scheduledAt
	}
	if speedLimitMbps != nil {
		sched.SpeedLimitMbps = *speedLimitMbps
	}
	// Reset to scheduled if it was errored (reschedule)
	if sched.Status == ScheduleStatusError {
		sched.Status = ScheduleStatusScheduled
		sched.Error = ""
	}
	cp := *sched
	s.mu.Unlock()

	s.saveSchedules()
	log.Printf("Schedule updated: %s at %s (limit: %.0f Mbps)", id, cp.ScheduledAt.Format(time.RFC3339), cp.SpeedLimitMbps)
	return &cp, nil
}

func (s *Scheduler) CancelSchedule(id string) error {
	s.mu.Lock()
	sched, ok := s.schedules[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("schedule not found: %s", id)
	}
	if sched.Status != ScheduleStatusScheduled && sched.Status != ScheduleStatusRunning {
		s.mu.Unlock()
		return fmt.Errorf("schedule not in cancellable state: %s", sched.Status)
	}
	downloadID := sched.DownloadID
	sched.Status = ScheduleStatusCancelled
	s.mu.Unlock()

	// Cancel the underlying download if running
	if downloadID != "" {
		_ = s.manager.CancelDownload(downloadID)
	}

	s.saveSchedules()
	return nil
}

func (s *Scheduler) RemoveSchedule(id string) error {
	s.mu.Lock()
	sched, ok := s.schedules[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("schedule not found: %s", id)
	}
	downloadID := sched.DownloadID
	status := sched.Status
	delete(s.schedules, id)
	s.mu.Unlock()

	// Only clean up the underlying download if the schedule hasn't completed
	if downloadID != "" && status != ScheduleStatusCompleted {
		_ = s.manager.CancelDownload(downloadID)
		_ = s.manager.RemoveDownload(downloadID)
	}

	s.saveSchedules()
	return nil
}

func (s *Scheduler) GetSchedules() []ScheduledDownload {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]ScheduledDownload, 0, len(s.schedules))
	for _, sched := range s.schedules {
		items = append(items, *sched)
	}
	return items
}

func (s *Scheduler) GetSchedule(id string) (*ScheduledDownload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sched, ok := s.schedules[id]
	if !ok {
		return nil, fmt.Errorf("schedule not found: %s", id)
	}
	cp := *sched
	return &cp, nil
}

// run checks every 30 seconds for due schedules.
func (s *Scheduler) run() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.checkSchedules()
		}
	}
}

func (s *Scheduler) checkSchedules() {
	now := time.Now()
	s.mu.RLock()
	var due []*ScheduledDownload
	for _, sched := range s.schedules {
		if sched.Status == ScheduleStatusScheduled && !now.Before(sched.ScheduledAt) {
			due = append(due, sched)
		}
	}
	s.mu.RUnlock()

	for _, sched := range due {
		s.executeSchedule(sched)
	}
}

func (s *Scheduler) executeSchedule(sched *ScheduledDownload) {
	s.mu.Lock()
	sched.Status = ScheduleStatusRunning
	s.mu.Unlock()
	s.saveSchedules()

	log.Printf("Executing scheduled download: %s (limit: %.0f Mbps)", sched.ID, sched.SpeedLimitMbps)

	// Save and set speed limit for this schedule
	s.prevLimitMu.Lock()
	s.prevLimit = s.manager.cfg.SpeedLimitMbps
	s.prevLimitMu.Unlock()

	if sched.SpeedLimitMbps > 0 {
		s.manager.engine.SetSpeedLimit(sched.SpeedLimitMbps)
		s.manager.cfg.SpeedLimitMbps = sched.SpeedLimitMbps
	}

	// Start the download
	var item *DownloadItem
	var err error

	source := sched.Source
	switch {
	case strings.HasPrefix(source, "magnet:"):
		item, err = s.manager.AddMagnet(source, sched.Category)
	case strings.Contains(source, "real-debrid.com/d/"):
		item, err = s.manager.AddRDLink(source, sched.Category, sched.Folder)
	case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
		name := "scheduled-download"
		item, err = s.manager.AddDirectURL(source, name, sched.Category)
	default:
		err = fmt.Errorf("unsupported source: %s", source)
	}

	if err != nil {
		s.mu.Lock()
		sched.Status = ScheduleStatusError
		sched.Error = err.Error()
		s.mu.Unlock()
		s.restoreLimit()
		s.saveSchedules()
		return
	}

	s.mu.Lock()
	sched.DownloadID = item.ID
	s.mu.Unlock()
	s.saveSchedules()

	// Watch the download and restore limit when done
	go s.watchDownload(sched, item.ID)
}

func (s *Scheduler) watchDownload(sched *ScheduledDownload, downloadID string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			item, err := s.manager.GetDownload(downloadID)
			if err != nil {
				continue
			}

			// Update schedule name and category from resolved download
			if item.Name != "" && item.Name != "Resolving magnet..." && item.Name != "Resolving RD link..." {
				s.mu.Lock()
				changed := false
				if sched.Name == "" || sched.Name != item.Name {
					sched.Name = item.Name
					changed = true
				}
				if item.Category != "" && sched.Category != item.Category {
					sched.Category = item.Category
					changed = true
				}
				s.mu.Unlock()
				if changed {
					s.saveSchedules()
				}
			}

			switch item.Status {
			case StatusCompleted:
				now := time.Now()
				s.mu.Lock()
				sched.Status = ScheduleStatusCompleted
				sched.CompletedAt = &now
				s.mu.Unlock()
				s.restoreLimit()
				s.saveSchedules()
				log.Printf("Scheduled download completed: %s", sched.ID)
				return
			case StatusError, StatusCancelled:
				s.mu.Lock()
				sched.Status = ScheduleStatusError
				sched.Error = item.Error
				if item.Status == StatusCancelled {
					sched.Error = "download cancelled"
				}
				s.mu.Unlock()
				s.restoreLimit()
				s.saveSchedules()
				return
			}
		}
	}
}

func (s *Scheduler) restoreLimit() {
	s.prevLimitMu.Lock()
	prev := s.prevLimit
	s.prevLimitMu.Unlock()

	s.manager.engine.SetSpeedLimit(prev)
	s.manager.cfg.SpeedLimitMbps = prev
	s.manager.cfg.SaveSettings()
	log.Printf("Speed limit restored to %.0f Mbps", prev)
}

func (s *Scheduler) saveSchedules() {
	s.mu.RLock()
	items := make([]ScheduledDownload, 0, len(s.schedules))
	for _, sched := range s.schedules {
		items = append(items, *sched)
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.scheduleFile), 0755)
	_ = os.WriteFile(s.scheduleFile, data, 0644)
}

func (s *Scheduler) loadSchedules() {
	data, err := os.ReadFile(s.scheduleFile)
	if err != nil {
		return
	}
	var items []ScheduledDownload
	if err := json.Unmarshal(data, &items); err != nil {
		return
	}
	for i := range items {
		item := items[i]
		if item.Status == ScheduleStatusRunning {
			item.Status = ScheduleStatusRunning // keep as running for retryInterrupted
		}
		s.schedules[item.ID] = &item
	}
}

// retryInterrupted handles schedules that were running when the service restarted.
// If the download is already being auto-resumed by the manager, just watch it.
// Otherwise, re-execute from scratch.
func (s *Scheduler) retryInterrupted() {
	s.mu.RLock()
	var interrupted []*ScheduledDownload
	for _, sched := range s.schedules {
		if sched.Status == ScheduleStatusRunning {
			interrupted = append(interrupted, sched)
		}
	}
	s.mu.RUnlock()

	if len(interrupted) == 0 {
		return
	}

	// Delay to let the manager's auto-resume finish first
	go func() {
		time.Sleep(5 * time.Second)
		for _, sched := range interrupted {
			// Check if the manager already has and is resuming this download
			if sched.DownloadID != "" {
				if dl, err := s.manager.GetDownload(sched.DownloadID); err == nil {
					// Download exists — just watch it instead of creating a new one
					log.Printf("Schedule %s: download %s already resuming (status: %s), watching", sched.ID, sched.DownloadID, dl.Status)

					// Apply speed limit if needed
					if sched.SpeedLimitMbps > 0 {
						s.prevLimitMu.Lock()
						s.prevLimit = s.manager.cfg.SpeedLimitMbps
						s.prevLimitMu.Unlock()
						s.manager.engine.SetSpeedLimit(sched.SpeedLimitMbps)
						s.manager.cfg.SpeedLimitMbps = sched.SpeedLimitMbps
					}

					go s.watchDownload(sched, sched.DownloadID)
					s.saveSchedules()
					continue
				}
			}

			// No existing download — re-execute from scratch
			log.Printf("Re-executing interrupted schedule: %s", sched.ID)
			s.mu.Lock()
			sched.Status = ScheduleStatusScheduled
			sched.Error = ""
			s.mu.Unlock()
			s.executeSchedule(sched)
		}
	}()
}
