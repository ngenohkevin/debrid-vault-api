package config

import (
	"os"
	"strconv"
)

type Config struct {
	Port                   string
	RDApiKey               string
	DownloadDir            string
	MoviesDir              string
	TVShowsDir             string
	AllowedOrigins         string
	APIKey                 string
	MaxConcurrentDownloads int
	MaxSegmentsPerFile     int
	SpeedLimitMbps         float64
}

func Load() *Config {
	return &Config{
		Port:                   getEnv("PORT", "6501"),
		RDApiKey:               getEnv("RD_API_KEY", ""),
		DownloadDir:            getEnv("DOWNLOAD_DIR", "/home/ngenoh/downloads/staging"),
		MoviesDir:              getEnv("MOVIES_DIR", "/mnt/perigrine/media/movies"),
		TVShowsDir:             getEnv("TVSHOWS_DIR", "/mnt/perigrine/media/tv-shows"),
		AllowedOrigins:         getEnv("ALLOWED_ORIGINS", "*"),
		APIKey:                 getEnv("API_KEY", ""),
		MaxConcurrentDownloads: getEnvInt("MAX_CONCURRENT_DOWNLOADS", 4),
		MaxSegmentsPerFile:     getEnvInt("MAX_SEGMENTS_PER_FILE", 8),
		SpeedLimitMbps:         getEnvFloat("SPEED_LIMIT_MBPS", 0),
	}
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
