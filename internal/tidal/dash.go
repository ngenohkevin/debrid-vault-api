package tidal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DASHManifest holds the parsed DASH XML data.
type DASHManifest struct {
	BaseURL       string
	InitTemplate  string
	MediaTemplate string
	Segments      []DASHSegment
	RepID         string
	MimeType      string
	Codec         string
}

type DASHSegment struct {
	Number int
	Time   int64
}

// directManifest is the JSON format for LOSSLESS (non-DASH) streams.
type directManifest struct {
	MimeType string   `json:"mimeType"`
	Codecs   string   `json:"codecs"`
	URLs     []string `json:"urls"`
}

// ProgressFunc reports download progress for a Tidal track.
type ProgressFunc func(downloaded, total int64)

// DownloadTrackAudio downloads a track's audio to destPath as FLAC.
// Tries HI_RES_LOSSLESS first, falls back to LOSSLESS.
func (c *Client) DownloadTrackAudio(ctx context.Context, trackID, destPath string) (quality string, bitDepth int, sampleRate int, err error) {
	return c.DownloadTrackAudioWithProgress(ctx, trackID, destPath, nil)
}

// DownloadTrackAudioWithProgress downloads with optional progress reporting.
func (c *Client) DownloadTrackAudioWithProgress(ctx context.Context, trackID, destPath string, progress ProgressFunc) (quality string, bitDepth int, sampleRate int, err error) {
	// Try Hi-Res first
	for _, q := range []string{QualityHiResLossless, QualityLossless} {
		info, e := c.GetPlaybackInfo(trackID, q)
		if e != nil {
			log.Printf("Tidal playback %s for track %s: %v", q, trackID, e)
			continue
		}

		quality = info.AudioQuality
		bitDepth = info.BitDepth
		sampleRate = info.SampleRate

		if strings.Contains(info.ManifestMimeType, "dash") || strings.Contains(info.ManifestMimeType, "xml") {
			err = c.downloadDASH(ctx, info.Manifest, destPath, progress)
		} else {
			err = c.downloadDirect(ctx, info.Manifest, destPath, progress)
		}
		if err == nil {
			return quality, bitDepth, sampleRate, nil
		}
		log.Printf("Tidal download %s failed for track %s: %v, trying next quality", q, trackID, err)
	}

	if err == nil {
		err = fmt.Errorf("no playback info available for track %s", trackID)
	}
	return
}

func (c *Client) downloadDirect(ctx context.Context, base64Manifest, destPath string, progress ProgressFunc) error {
	decoded, err := base64.StdEncoding.DecodeString(base64Manifest)
	if err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}

	var m directManifest
	if err := json.Unmarshal(decoded, &m); err != nil {
		return fmt.Errorf("parse direct manifest: %w", err)
	}
	if len(m.URLs) == 0 {
		return fmt.Errorf("no URLs in manifest")
	}

	streamURL := m.URLs[0]

	req, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("download stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("stream returned %d", resp.StatusCode)
	}

	total := resp.ContentLength

	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var downloaded int64
	buf := make([]byte, 128*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := f.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			downloaded += int64(n)
			if progress != nil {
				progress(downloaded, total)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}
	return nil
}

func (c *Client) downloadDASH(ctx context.Context, base64Manifest, destPath string, progress ProgressFunc) error {
	decoded, err := base64.StdEncoding.DecodeString(base64Manifest)
	if err != nil {
		return fmt.Errorf("decode DASH manifest: %w", err)
	}

	manifest, err := parseDASHXML(decoded)
	if err != nil {
		return fmt.Errorf("parse DASH: %w", err)
	}

	urls := manifest.buildSegmentURLs()
	if len(urls) == 0 {
		return fmt.Errorf("no segment URLs in DASH manifest")
	}

	// Download all segments to a temp file
	tmpDir := filepath.Dir(destPath)
	segFile, err := os.CreateTemp(tmpDir, "tidal-segments-*.mp4")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	segPath := segFile.Name()
	defer os.Remove(segPath)

	client := &http.Client{Timeout: 120 * time.Second}
	totalSegments := int64(len(urls))
	for i, u := range urls {
		select {
		case <-ctx.Done():
			segFile.Close()
			return ctx.Err()
		default:
		}

		// Retry each segment up to 3 times
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
			}

			req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
			if err != nil {
				segFile.Close()
				return err
			}

			resp, err := client.Do(req)
			if err != nil {
				lastErr = fmt.Errorf("segment %d/%d attempt %d: %w", i+1, len(urls), attempt+1, err)
				continue
			}

			_, err = io.Copy(segFile, resp.Body)
			resp.Body.Close()
			if err != nil {
				lastErr = fmt.Errorf("segment %d write: %w", i+1, err)
				continue
			}

			lastErr = nil
			break
		}
		if lastErr != nil {
			segFile.Close()
			return lastErr
		}

		// Report progress as segment count
		if progress != nil {
			progress(int64(i+1), totalSegments)
		}
	}
	segFile.Close()

	// Remux to FLAC with ffmpeg
	// If codec is FLAC, use -c:a copy; if ALAC, transcode to FLAC
	codecArg := "copy"
	if strings.Contains(strings.ToLower(manifest.Codec), "alac") ||
		strings.Contains(strings.ToLower(manifest.MimeType), "mp4") {
		// MP4 container may have ALAC or FLAC — try copy first
		codecArg = "copy"
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", "-y",
		"-i", segPath,
		"-c:a", codecArg,
		"-vn", // no video
		destPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// If copy failed (e.g. ALAC), try transcoding to FLAC
		if codecArg == "copy" {
			log.Printf("ffmpeg copy failed, trying transcode: %s", string(output))
			cmd2 := exec.CommandContext(ctx, "ffmpeg", "-y",
				"-i", segPath,
				"-c:a", "flac",
				"-vn",
				destPath,
			)
			output2, err2 := cmd2.CombinedOutput()
			if err2 != nil {
				return fmt.Errorf("ffmpeg transcode failed: %v — %s", err2, string(output2))
			}
			return nil
		}
		return fmt.Errorf("ffmpeg remux failed: %v — %s", err, string(output))
	}
	return nil
}

// --- DASH XML Parsing ---

type mpd struct {
	XMLName xml.Name    `xml:"MPD"`
	Period  []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	AdaptationSet []mpdAdaptationSet `xml:"AdaptationSet"`
	BaseURL       string             `xml:"BaseURL"`
}

type mpdAdaptationSet struct {
	MimeType        string              `xml:"mimeType,attr"`
	Representation  []mpdRepresentation `xml:"Representation"`
	SegmentTemplate *mpdSegmentTemplate `xml:"SegmentTemplate"`
	BaseURL         string              `xml:"BaseURL"`
}

type mpdRepresentation struct {
	ID              string              `xml:"id,attr"`
	Bandwidth       int                 `xml:"bandwidth,attr"`
	Codecs          string              `xml:"codecs,attr"`
	SegmentTemplate *mpdSegmentTemplate `xml:"SegmentTemplate"`
	BaseURL         string              `xml:"BaseURL"`
}

type mpdSegmentTemplate struct {
	Initialization  string              `xml:"initialization,attr"`
	Media           string              `xml:"media,attr"`
	StartNumber     int                 `xml:"startNumber,attr"`
	SegmentTimeline *mpdSegmentTimeline `xml:"SegmentTimeline"`
}

type mpdSegmentTimeline struct {
	S []mpdS `xml:"S"`
}

type mpdS struct {
	T int64 `xml:"t,attr"` // time
	D int64 `xml:"d,attr"` // duration
	R int   `xml:"r,attr"` // repeat count
}

func parseDASHXML(data []byte) (*DASHManifest, error) {
	var m mpd
	if err := xml.Unmarshal(data, &m); err != nil {
		return nil, err
	}

	if len(m.Period) == 0 {
		return nil, fmt.Errorf("no Period in DASH manifest")
	}

	period := m.Period[0]

	// Find the audio AdaptationSet with highest bandwidth
	var bestSet *mpdAdaptationSet
	var bestRep *mpdRepresentation
	bestBandwidth := 0

	for i := range period.AdaptationSet {
		as := &period.AdaptationSet[i]
		if as.MimeType != "" && !strings.HasPrefix(as.MimeType, "audio") {
			continue
		}
		for j := range as.Representation {
			rep := &as.Representation[j]
			if rep.Bandwidth > bestBandwidth {
				bestBandwidth = rep.Bandwidth
				bestSet = as
				bestRep = rep
			}
		}
	}

	if bestRep == nil {
		// Fallback: use first set
		if len(period.AdaptationSet) > 0 && len(period.AdaptationSet[0].Representation) > 0 {
			bestSet = &period.AdaptationSet[0]
			bestRep = &period.AdaptationSet[0].Representation[0]
		} else {
			return nil, fmt.Errorf("no Representation found in DASH manifest")
		}
	}

	// Find SegmentTemplate (can be on Representation or AdaptationSet level)
	segTpl := bestRep.SegmentTemplate
	if segTpl == nil {
		segTpl = bestSet.SegmentTemplate
	}
	if segTpl == nil {
		return nil, fmt.Errorf("no SegmentTemplate in DASH manifest")
	}

	// Determine base URL
	baseURL := ""
	if bestRep.BaseURL != "" {
		baseURL = bestRep.BaseURL
	} else if bestSet.BaseURL != "" {
		baseURL = bestSet.BaseURL
	} else if period.BaseURL != "" {
		baseURL = period.BaseURL
	}

	result := &DASHManifest{
		BaseURL:       baseURL,
		InitTemplate:  segTpl.Initialization,
		MediaTemplate: segTpl.Media,
		RepID:         bestRep.ID,
		MimeType:      bestSet.MimeType,
		Codec:         bestRep.Codecs,
	}

	// Parse SegmentTimeline
	if segTpl.SegmentTimeline != nil {
		currentTime := int64(0)
		currentNumber := segTpl.StartNumber
		if currentNumber == 0 {
			currentNumber = 1
		}

		for _, s := range segTpl.SegmentTimeline.S {
			if s.T != 0 {
				currentTime = s.T
			}

			result.Segments = append(result.Segments, DASHSegment{
				Number: currentNumber,
				Time:   currentTime,
			})
			currentTime += s.D
			currentNumber++

			for i := 0; i < s.R; i++ {
				result.Segments = append(result.Segments, DASHSegment{
					Number: currentNumber,
					Time:   currentTime,
				})
				currentTime += s.D
				currentNumber++
			}
		}
	}

	return result, nil
}

var (
	repIDRegex  = regexp.MustCompile(`\$RepresentationID\$`)
	numberRegex = regexp.MustCompile(`\$Number(%0(\d+)d)?\$`)
	timeRegex   = regexp.MustCompile(`\$Time(%0(\d+)d)?\$`)
)

func (m *DASHManifest) buildSegmentURLs() []string {
	var urls []string

	resolveTemplate := func(tpl string, number int, t int64) string {
		s := repIDRegex.ReplaceAllString(tpl, m.RepID)
		s = numberRegex.ReplaceAllStringFunc(s, func(match string) string {
			sub := numberRegex.FindStringSubmatch(match)
			if len(sub) > 2 && sub[2] != "" {
				w, _ := strconv.Atoi(sub[2])
				return fmt.Sprintf("%0*d", w, number)
			}
			return strconv.Itoa(number)
		})
		s = timeRegex.ReplaceAllStringFunc(s, func(match string) string {
			sub := timeRegex.FindStringSubmatch(match)
			if len(sub) > 2 && sub[2] != "" {
				w, _ := strconv.Atoi(sub[2])
				return fmt.Sprintf("%0*d", w, t)
			}
			return strconv.FormatInt(t, 10)
		})
		return s
	}

	joinPath := func(base, part string) string {
		if base == "" {
			return part
		}
		if strings.HasPrefix(part, "http") {
			return part
		}
		if strings.HasSuffix(base, "/") {
			return base + part
		}
		return base + "/" + part
	}

	if m.InitTemplate != "" {
		initURL := resolveTemplate(m.InitTemplate, 0, 0)
		urls = append(urls, joinPath(m.BaseURL, initURL))
	}

	if m.MediaTemplate != "" {
		for _, seg := range m.Segments {
			mediaURL := resolveTemplate(m.MediaTemplate, seg.Number, seg.Time)
			urls = append(urls, joinPath(m.BaseURL, mediaURL))
		}
	}

	return urls
}
