package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Port                   string
	RDApiKey               string
	TBApiKey               string
	DownloadDir            string
	MoviesDir              string
	TVShowsDir             string
	MusicDir               string
	AllowedOrigins         string
	APIKey                 string
	MaxConcurrentDownloads int
	MaxSegmentsPerFile     int
	SpeedLimitMbps         float64
}

// persistedSettings is the subset of config saved to disk.
type persistedSettings struct {
	SpeedLimitMbps float64 `json:"speedLimitMbps"`
}

func Load() *Config {
	cfg := &Config{
		Port:                   getEnv("PORT", "6501"),
		RDApiKey:               getEnv("RD_API_KEY", ""),
		TBApiKey:               getEnv("TB_API_KEY", ""),
		DownloadDir:            getEnv("DOWNLOAD_DIR", "/home/ngenoh/downloads/staging"),
		MoviesDir:              getEnv("MOVIES_DIR", "/mnt/perigrine/media/movies"),
		TVShowsDir:             getEnv("TVSHOWS_DIR", "/mnt/perigrine/media/tv-shows"),
		MusicDir:               getEnv("MUSIC_DIR", "/mnt/perigrine/media/music"),
		AllowedOrigins:         getEnv("ALLOWED_ORIGINS", "*"),
		APIKey:                 getEnv("API_KEY", ""),
		MaxConcurrentDownloads: getEnvInt("MAX_CONCURRENT_DOWNLOADS", 4),
		MaxSegmentsPerFile:     getEnvInt("MAX_SEGMENTS_PER_FILE", 8),
		SpeedLimitMbps:         getEnvFloat("SPEED_LIMIT_MBPS", 0),
	}
	cfg.loadPersistedSettings()
	return cfg
}

func (c *Config) settingsPath() string {
	return filepath.Join(c.DownloadDir, ".settings.json")
}

func (c *Config) loadPersistedSettings() {
	data, err := os.ReadFile(c.settingsPath())
	if err != nil {
		return
	}
	var s persistedSettings
	if json.Unmarshal(data, &s) == nil {
		if s.SpeedLimitMbps > 0 {
			c.SpeedLimitMbps = s.SpeedLimitMbps
		}
	}
}

// SaveSettings persists user-configurable settings to disk.
func (c *Config) SaveSettings() {
	s := persistedSettings{
		SpeedLimitMbps: c.SpeedLimitMbps,
	}
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.settingsPath()), 0755)
	_ = os.WriteFile(c.settingsPath(), data, 0644)
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		if n, err := strconv.Atoi(val); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if val := os.Getenv(key); val != "" {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
	}
	return fallback
}
