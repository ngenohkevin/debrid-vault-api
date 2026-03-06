package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/time/rate"
)

// SegmentState tracks the byte range and progress for a single download segment.
type SegmentState struct {
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	Downloaded int64 `json:"downloaded"`
}

// PartFile is the JSON metadata persisted for resume/pause support.
type PartFile struct {
	URL       string         `json:"url"`
	TotalSize int64          `json:"totalSize"`
	Segments  []SegmentState `json:"segments"`
	RangeOK   bool           `json:"rangeOK"`
}

// ProgressCallback reports aggregated download progress.
type ProgressCallback func(downloaded, total, speed int64)

// StatusCallback reports engine status messages (retries, errors, etc.)
type StatusCallback func(msg string)

// DownloadEngine is the multi-segment download core (IDM-style).
type DownloadEngine struct {
	transport   *http.Transport
	bufPool     sync.Pool
	maxSegments int
	limiter     *rate.Limiter
	limiterMu   sync.RWMutex
}

const (
	segmentBufSize   = 128 * 1024 // 128KB per segment read buffer
	partSaveInterval = 5 * time.Second
	moveBufSize      = 1024 * 1024 // 1MB for file moves
)

// NewDownloadEngine creates a new multi-segment download engine.
func NewDownloadEngine(maxSegments int, speedLimitMbps float64) *DownloadEngine {
	e := &DownloadEngine{
		transport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
		bufPool: sync.Pool{
			New: func() any {
				buf := make([]byte, segmentBufSize)
				return &buf
			},
		},
		maxSegments: maxSegments,
	}
	e.SetSpeedLimit(speedLimitMbps)
	return e
}

// SetSpeedLimit sets the global bandwidth limit in Mbps. 0 = unlimited.
func (e *DownloadEngine) SetSpeedLimit(mbps float64) {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	if mbps <= 0 {
		e.limiter = nil
		return
	}
	bytesPerSec := mbps * 1_000_000 / 8
	e.limiter = rate.NewLimiter(rate.Limit(bytesPerSec), moveBufSize) // burst = 1MB
}

func (e *DownloadEngine) getLimiter() *rate.Limiter {
	e.limiterMu.RLock()
	defer e.limiterMu.RUnlock()
	return e.limiter
}

// Download is the main entry point for multi-segment downloading.
func (e *DownloadEngine) Download(ctx context.Context, url, destPath string, numSegments int, progress ProgressCallback, status StatusCallback) error {
	totalSize, rangeOK, err := e.probeURL(ctx, url)
	if err != nil {
		return fmt.Errorf("probe failed: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	if totalSize > 0 {
		if err := e.checkDiskSpace(filepath.Dir(destPath), totalSize); err != nil {
			return err
		}
	}

	partPath := destPath + ".part"

	// Try to load existing .part file for resume
	pf, loaded := e.loadPartFile(partPath)
	if loaded && pf.URL == url && pf.TotalSize == totalSize && pf.RangeOK == rangeOK {
		log.Printf("Resuming download from .part file: %s", partPath)
	} else {
		// Fresh download
		if !rangeOK || totalSize <= 0 {
			// No range support — single stream
			return e.downloadSingle(ctx, url, destPath, partPath, totalSize, progress, status)
		}
		pf = e.createPartFile(url, totalSize, numSegments)
	}

	// Pre-allocate the file
	file, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	if err := file.Truncate(totalSize); err != nil {
		file.Close()
		return fmt.Errorf("pre-allocate: %w", err)
	}

	// Save initial .part
	e.savePartFile(partPath, pf)

	// Launch segments
	var wg sync.WaitGroup
	segErrors := make([]error, len(pf.Segments))
	atomicCounters := make([]atomic.Int64, len(pf.Segments))

	// Initialize counters with already-downloaded bytes
	for i := range pf.Segments {
		atomicCounters[i].Store(pf.Segments[i].Downloaded)
	}

	for i := range pf.Segments {
		if pf.Segments[i].Downloaded >= (pf.Segments[i].End - pf.Segments[i].Start + 1) {
			continue // segment already complete
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			segErrors[idx] = e.downloadSegment(ctx, file, &pf.Segments[idx], &atomicCounters[idx], url, idx, status)
		}(i)
	}

	// Progress reporter + periodic .part saver
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(500 * time.Millisecond)
		saveTicker := time.NewTicker(partSaveInterval)
		defer ticker.Stop()
		defer saveTicker.Stop()

		// Rolling speed window: 6 samples x 500ms = 3 second window
		type sample struct {
			bytes int64
			time  time.Time
		}
		var samples []sample
		const maxSamples = 6

		for {
			select {
			case <-ctx.Done():
				// Save .part on cancellation (pause)
				for i := range pf.Segments {
					pf.Segments[i].Downloaded = atomicCounters[i].Load()
				}
				e.savePartFile(partPath, pf)
				return
			case <-saveTicker.C:
				for i := range pf.Segments {
					pf.Segments[i].Downloaded = atomicCounters[i].Load()
				}
				e.savePartFile(partPath, pf)
			case <-ticker.C:
				var total int64
				for i := range atomicCounters {
					total += atomicCounters[i].Load()
				}

				now := time.Now()
				samples = append(samples, sample{bytes: total, time: now})
				if len(samples) > maxSamples {
					samples = samples[1:]
				}

				var speed int64
				if len(samples) >= 2 {
					oldest := samples[0]
					elapsed := now.Sub(oldest.time).Seconds()
					if elapsed > 0 {
						speed = int64(float64(total-oldest.bytes) / elapsed)
					}
				}

				if progress != nil {
					progress(total, totalSize, speed)
				}

				if total >= totalSize {
					return
				}
			}
		}
	}()

	wg.Wait()
	<-done

	// Check for context cancellation (pause)
	if ctx.Err() != nil {
		file.Close()
		return ctx.Err()
	}

	// Check segment errors
	for i, err := range segErrors {
		if err != nil {
			file.Close()
			return fmt.Errorf("segment %d failed: %w", i, err)
		}
	}

	// Verify and finalize
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync: %w", err)
	}
	file.Close()

	// Verify file size
	info, err := os.Stat(destPath)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() != totalSize {
		return fmt.Errorf("size mismatch: got %d, expected %d", info.Size(), totalSize)
	}

	// Clean up .part file
	os.Remove(partPath)

	// Report final progress
	if progress != nil {
		progress(totalSize, totalSize, 0)
	}

	return nil
}

// segmentBackoffs defines the retry schedule for a single segment.
// Total wait: ~30 minutes before giving up — survives most internet outages.
var segmentBackoffs = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	1 * time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	5 * time.Minute,
	5 * time.Minute,
	5 * time.Minute,
}

// downloadSegment downloads a single byte range with extended retry.
func (e *DownloadEngine) downloadSegment(ctx context.Context, file *os.File, seg *SegmentState, counter *atomic.Int64, url string, segIdx int, status StatusCallback) error {
	segLen := seg.End - seg.Start + 1
	if seg.Downloaded >= segLen {
		return nil
	}

	emitStatus := func(msg string) {
		if status != nil {
			status(msg)
		}
	}

	var lastErr error

	for attempt := 0; attempt <= len(segmentBackoffs); attempt++ {
		if attempt > 0 {
			backoff := segmentBackoffs[attempt-1]
			msg := fmt.Sprintf("Segment %d: retry %d/%d in %s — %v", segIdx+1, attempt, len(segmentBackoffs), backoff, lastErr)
			log.Print(msg)
			emitStatus(msg)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			emitStatus(fmt.Sprintf("Segment %d: retrying...", segIdx+1))
		}

		lastErr = e.downloadSegmentOnce(ctx, file, seg, counter, url)
		if lastErr == nil {
			if attempt > 0 {
				emitStatus(fmt.Sprintf("Segment %d: recovered after %d retries", segIdx+1, attempt))
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("segment %d failed after %d retries over ~30min: %w", segIdx+1, len(segmentBackoffs), lastErr)
}

func (e *DownloadEngine) downloadSegmentOnce(ctx context.Context, file *os.File, seg *SegmentState, counter *atomic.Int64, url string) error {
	segLen := seg.End - seg.Start + 1
	currentOffset := seg.Start + seg.Downloaded

	if seg.Downloaded >= segLen {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", currentOffset, seg.End))

	client := &http.Client{Transport: e.transport, Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for range %d-%d", resp.StatusCode, currentOffset, seg.End)
	}

	// Get buffer from pool
	bufPtr := e.bufPool.Get().(*[]byte)
	buf := *bufPtr
	defer e.bufPool.Put(bufPtr)

	limiter := e.getLimiter()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			// Rate limit
			if limiter != nil {
				if err := limiter.WaitN(ctx, n); err != nil {
					return err
				}
			}

			_, writeErr := file.WriteAt(buf[:n], currentOffset)
			if writeErr != nil {
				return writeErr
			}
			currentOffset += int64(n)
			seg.Downloaded += int64(n)
			counter.Store(seg.Downloaded)
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

// downloadSingle is the fallback for servers that don't support Range.
func (e *DownloadEngine) downloadSingle(ctx context.Context, url, destPath, partPath string, totalSize int64, progress ProgressCallback, status StatusCallback) error {
	pf := &PartFile{
		URL:       url,
		TotalSize: totalSize,
		Segments:  []SegmentState{{Start: 0, End: totalSize - 1, Downloaded: 0}},
		RangeOK:   false,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Transport: e.transport, Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	if totalSize <= 0 {
		totalSize = resp.ContentLength
		pf.TotalSize = totalSize
	}

	file, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer file.Close()

	bufPtr := e.bufPool.Get().(*[]byte)
	buf := *bufPtr
	defer e.bufPool.Put(bufPtr)

	var downloaded int64
	lastReport := time.Now()
	lastReportBytes := int64(0)
	saveTicker := time.NewTicker(partSaveInterval)
	defer saveTicker.Stop()

	limiter := e.getLimiter()

	// Rolling speed samples
	type sample struct {
		bytes int64
		time  time.Time
	}
	var samples []sample
	const maxSamples = 6

	for {
		select {
		case <-ctx.Done():
			pf.Segments[0].Downloaded = downloaded
			e.savePartFile(partPath, pf)
			return ctx.Err()
		default:
		}

		// Check if we should save .part
		select {
		case <-saveTicker.C:
			pf.Segments[0].Downloaded = downloaded
			e.savePartFile(partPath, pf)
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if limiter != nil {
				if err := limiter.WaitN(ctx, n); err != nil {
					return err
				}
			}

			_, writeErr := file.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			downloaded += int64(n)

			now := time.Now()
			if progress != nil && now.Sub(lastReport) >= 500*time.Millisecond {
				samples = append(samples, sample{bytes: downloaded, time: now})
				if len(samples) > maxSamples {
					samples = samples[1:]
				}
				var speed int64
				if len(samples) >= 2 {
					oldest := samples[0]
					elapsed := now.Sub(oldest.time).Seconds()
					if elapsed > 0 {
						speed = int64(float64(downloaded-oldest.bytes) / elapsed)
					}
				}
				progress(downloaded, totalSize, speed)
				lastReport = now
				lastReportBytes = downloaded
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	_ = lastReportBytes

	file.Sync()
	os.Remove(partPath)

	if progress != nil {
		progress(downloaded, totalSize, 0)
	}
	return nil
}

// probeURL sends a HEAD request to determine file size and Range support.
func (e *DownloadEngine) probeURL(ctx context.Context, url string) (size int64, rangeOK bool, err error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return 0, false, err
	}

	client := &http.Client{Transport: e.transport, Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return 0, false, fmt.Errorf("HEAD returned HTTP %d", resp.StatusCode)
	}

	size = resp.ContentLength
	rangeOK = resp.Header.Get("Accept-Ranges") == "bytes"
	return size, rangeOK, nil
}

// checkDiskSpace verifies sufficient disk space is available.
func (e *DownloadEngine) checkDiskSpace(dir string, required int64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	if available < required {
		return fmt.Errorf("insufficient disk space: need %d bytes, have %d", required, available)
	}
	return nil
}

func (e *DownloadEngine) createPartFile(url string, totalSize int64, numSegments int) *PartFile {
	if numSegments < 1 {
		numSegments = 1
	}
	if numSegments > e.maxSegments {
		numSegments = e.maxSegments
	}

	segSize := totalSize / int64(numSegments)
	segments := make([]SegmentState, numSegments)
	for i := range segments {
		segments[i].Start = int64(i) * segSize
		if i == numSegments-1 {
			segments[i].End = totalSize - 1
		} else {
			segments[i].End = int64(i+1)*segSize - 1
		}
	}

	return &PartFile{
		URL:       url,
		TotalSize: totalSize,
		Segments:  segments,
		RangeOK:   true,
	}
}

func (e *DownloadEngine) loadPartFile(path string) (*PartFile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var pf PartFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, false
	}
	return &pf, true
}

func (e *DownloadEngine) savePartFile(path string, pf *PartFile) {
	data, err := json.Marshal(pf)
	if err != nil {
		return
	}
	// Write to temp file then rename for atomicity
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// CopyFileBuffered copies src to dst using a 1MB buffer and reports progress.
func CopyFileBuffered(ctx context.Context, src, dst string, progress func(copied, total int64)) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	total := info.Size()

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

	buf := make([]byte, moveBufSize)
	var copied int64
	lastReport := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, readErr := in.Read(buf)
		if n > 0 {
			_, writeErr := out.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			copied += int64(n)

			if progress != nil && time.Since(lastReport) >= 500*time.Millisecond {
				progress(copied, total)
				lastReport = time.Now()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	if progress != nil {
		progress(total, total)
	}
	return out.Close()
}
