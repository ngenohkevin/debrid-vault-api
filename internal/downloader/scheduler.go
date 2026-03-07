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
	GroupID        string         `json:"groupId,omitempty"`
	ResumeIDs      []string       `json:"resumeIds,omitempty"`
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

// ScheduleExisting pauses an active download (or group) and schedules it to resume later.
func (s *Scheduler) ScheduleExisting(downloadID string, scheduledAt time.Time, speedLimitMbps float64) (*ScheduledDownload, error) {
	item, err := s.manager.GetDownload(downloadID)
	if err != nil {
		return nil, err
	}

	// Collect all IDs to pause and resume (single or group)
	var resumeIDs []string
	var name string
	var groupID string

	if item.GroupID != "" {
		// Group download — pause and schedule all items
		groupID = item.GroupID
		name = item.GroupName
		items := s.manager.GetDownloadsByGroup(item.GroupID)
		for _, gi := range items {
			if err := s.manager.CheckResumable(gi.ID); err != nil {
				continue // skip non-resumable items (completed, etc.)
			}
			resumeIDs = append(resumeIDs, gi.ID)
		}
	} else {
		// Single download
		if err := s.manager.CheckResumable(downloadID); err != nil {
			return nil, err
		}
		name = item.Name
		resumeIDs = []string{downloadID}
	}

	if len(resumeIDs) == 0 {
		return nil, fmt.Errorf("no resumable downloads found")
	}

	// Pause all the downloads
	for _, id := range resumeIDs {
		_ = s.manager.PauseDownload(id)
	}

	sched := &ScheduledDownload{
		ID:             uuid.New().String()[:8],
		Name:           name,
		Source:         item.Source,
		Category:       item.Category,
		Folder:         item.Folder,
		ScheduledAt:    scheduledAt,
		SpeedLimitMbps: speedLimitMbps,
		Status:         ScheduleStatusScheduled,
		DownloadID:     downloadID,
		GroupID:        groupID,
		ResumeIDs:      resumeIDs,
		CreatedAt:      time.Now(),
	}

	s.mu.Lock()
	s.schedules[sched.ID] = sched
	s.mu.Unlock()

	s.saveSchedules()
	log.Printf("Scheduled existing download: %s (%d items) at %s", sched.ID, len(resumeIDs), scheduledAt.Format(time.RFC3339))
	return sched, nil
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
	groupID := sched.GroupID
	sched.Status = ScheduleStatusCancelled
	s.mu.Unlock()

	// Cancel group downloads if multi-file
	if groupID != "" {
		items := s.manager.GetDownloadsByGroup(groupID)
		for _, item := range items {
			_ = s.manager.CancelDownload(item.ID)
		}
	}

	// Cancel the original download if running
	if downloadID != "" {
		_ = s.manager.CancelDownload(downloadID)
	}

	s.restoreLimit()
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
	groupID := sched.GroupID
	status := sched.Status
	delete(s.schedules, id)
	s.mu.Unlock()

	// Only clean up underlying downloads if the schedule hasn't completed
	if status != ScheduleStatusCompleted {
		if groupID != "" {
			items := s.manager.GetDownloadsByGroup(groupID)
			for _, item := range items {
				_ = s.manager.CancelDownload(item.ID)
				_ = s.manager.RemoveDownload(item.ID)
			}
		}
		if downloadID != "" {
			_ = s.manager.CancelDownload(downloadID)
			_ = s.manager.RemoveDownload(downloadID)
		}
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
	resumeIDs := sched.ResumeIDs
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

	// Resume mode: resume existing paused downloads
	if len(resumeIDs) > 0 {
		s.executeResume(sched, resumeIDs)
		return
	}

	// New download mode
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
	go s.watchDownload(sched)
}

// executeResume resumes existing paused downloads.
func (s *Scheduler) executeResume(sched *ScheduledDownload, resumeIDs []string) {
	var resumed int
	var lastErr string

	for _, id := range resumeIDs {
		if err := s.manager.ResumeDownload(id); err != nil {
			log.Printf("Failed to resume %s: %v", id, err)
			lastErr = err.Error()
		} else {
			resumed++
		}
	}

	if resumed == 0 {
		s.mu.Lock()
		sched.Status = ScheduleStatusError
		sched.Error = fmt.Sprintf("failed to resume any downloads: %s", lastErr)
		s.mu.Unlock()
		s.restoreLimit()
		s.saveSchedules()
		return
	}

	log.Printf("Resumed %d/%d downloads for schedule %s", resumed, len(resumeIDs), sched.ID)

	// If it's a group, watch the group; otherwise watch the single download
	s.mu.RLock()
	groupID := sched.GroupID
	s.mu.RUnlock()

	if groupID != "" {
		go s.watchGroup(sched, groupID)
	} else if len(resumeIDs) == 1 {
		s.mu.Lock()
		sched.DownloadID = resumeIDs[0]
		s.mu.Unlock()
		s.saveSchedules()
		go s.watchDownload(sched)
	} else {
		// Multiple non-group items — watch them all
		go s.watchResumeIDs(sched, resumeIDs)
	}
}

// watchResumeIDs watches a set of individual download IDs until all are done.
func (s *Scheduler) watchResumeIDs(sched *ScheduledDownload, ids []string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			completed := 0
			errored := 0
			var lastErr string
			for _, id := range ids {
				item, err := s.manager.GetDownload(id)
				if err != nil {
					continue
				}
				switch item.Status {
				case StatusCompleted:
					completed++
				case StatusError:
					errored++
					lastErr = item.Error
				case StatusCancelled:
					errored++
				}
			}

			if completed+errored == len(ids) {
				if completed == len(ids) {
					now := time.Now()
					s.mu.Lock()
					sched.Status = ScheduleStatusCompleted
					sched.CompletedAt = &now
					s.mu.Unlock()
				} else {
					s.mu.Lock()
					sched.Status = ScheduleStatusError
					sched.Error = lastErr
					s.mu.Unlock()
				}
				s.restoreLimit()
				s.saveSchedules()
				return
			}
		}
	}
}

func (s *Scheduler) watchDownload(sched *ScheduledDownload) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	downloadID := sched.DownloadID
	notFoundCount := 0

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			// Check if the original download spawned a group (multi-file magnet)
			s.mu.RLock()
			groupID := sched.GroupID
			s.mu.RUnlock()

			if groupID != "" {
				// Track the group of downloads
				s.watchGroup(sched, groupID)
				return
			}

			item, err := s.manager.GetDownload(downloadID)
			if err != nil {
				notFoundCount++
				// The original item may have been replaced by group items
				// Check if a group was created with this download's ID as source
				if notFoundCount >= 3 {
					// Look for group items that the magnet spawned
					items := s.manager.GetDownloads()
					for _, it := range items {
						if it.GroupID != "" && it.Source != "" {
							// Found group items — store groupID and switch to group tracking
							s.mu.Lock()
							sched.GroupID = it.GroupID
							sched.Name = it.GroupName
							sched.Category = it.Category
							s.mu.Unlock()
							s.saveSchedules()
							s.watchGroup(sched, it.GroupID)
							return
						}
					}
				}
				continue
			}
			notFoundCount = 0

			// Check if this download now has a GroupID (magnet resolved to multi-file)
			if item.GroupID != "" {
				s.mu.Lock()
				sched.GroupID = item.GroupID
				sched.Name = item.GroupName
				sched.Category = item.Category
				s.mu.Unlock()
				s.saveSchedules()
				// Original item will be removed, switch to group tracking
				s.watchGroup(sched, item.GroupID)
				return
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

// watchGroup tracks a group of downloads until all are completed or any error/cancel occurs.
func (s *Scheduler) watchGroup(sched *ScheduledDownload, groupID string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			items := s.manager.GetDownloadsByGroup(groupID)
			if len(items) == 0 {
				continue
			}

			completed := 0
			var lastErr string
			cancelled := false
			for _, item := range items {
				switch item.Status {
				case StatusCompleted:
					completed++
				case StatusError:
					lastErr = item.Error
				case StatusCancelled:
					cancelled = true
				}
			}

			if cancelled {
				s.mu.Lock()
				sched.Status = ScheduleStatusError
				sched.Error = "download cancelled"
				s.mu.Unlock()
				s.restoreLimit()
				s.saveSchedules()
				return
			}

			if lastErr != "" && completed+countStatus(items, StatusError) == len(items) {
				// All done but some errored
				s.mu.Lock()
				sched.Status = ScheduleStatusError
				sched.Error = lastErr
				s.mu.Unlock()
				s.restoreLimit()
				s.saveSchedules()
				return
			}

			if completed == len(items) {
				now := time.Now()
				s.mu.Lock()
				sched.Status = ScheduleStatusCompleted
				sched.CompletedAt = &now
				s.mu.Unlock()
				s.restoreLimit()
				s.saveSchedules()
				log.Printf("Scheduled group download completed: %s (%d files)", sched.ID, len(items))
				return
			}
		}
	}
}

func countStatus(items []DownloadItem, status Status) int {
	n := 0
	for _, item := range items {
		if item.Status == status {
			n++
		}
	}
	return n
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

					go s.watchDownload(sched)
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
