package downloader

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
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
	go s.run()
	return s
}

func (s *Scheduler) Stop() {
	close(s.stopCh)
}

func (s *Scheduler) AddSchedule(name, source string, category Category, folder string, scheduledAt time.Time, speedLimitMbps float64) *ScheduledDownload {
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

func (s *Scheduler) CancelSchedule(id string) error {
	s.mu.Lock()
	sched, ok := s.schedules[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("schedule not found: %s", id)
	}
	if sched.Status != ScheduleStatusScheduled {
		s.mu.Unlock()
		return fmt.Errorf("schedule not in scheduled state: %s", sched.Status)
	}
	sched.Status = ScheduleStatusCancelled
	s.mu.Unlock()

	s.saveSchedules()
	return nil
}

func (s *Scheduler) RemoveSchedule(id string) error {
	s.mu.Lock()
	_, ok := s.schedules[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("schedule not found: %s", id)
	}
	delete(s.schedules, id)
	s.mu.Unlock()

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
			item.Status = ScheduleStatusError
			item.Error = "interrupted by restart"
		}
		s.schedules[item.ID] = &item
	}
}
