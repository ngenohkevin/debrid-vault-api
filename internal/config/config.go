package config

import "os"

type Config struct {
	Port           string
	RDApiKey       string
	DownloadDir    string
	MoviesDir      string
	TVShowsDir     string
	AllowedOrigins string
	APIKey         string
}

func Load() *Config {
	return &Config{
		Port:           getEnv("PORT", "6501"),
		RDApiKey:       getEnv("RD_API_KEY", ""),
		DownloadDir:    getEnv("DOWNLOAD_DIR", "/home/ngenoh/downloads/staging"),
		MoviesDir:      getEnv("MOVIES_DIR", "/mnt/perigrine/media/movies"),
		TVShowsDir:     getEnv("TVSHOWS_DIR", "/mnt/perigrine/media/tv-shows"),
		AllowedOrigins: getEnv("ALLOWED_ORIGINS", "*"),
		APIKey:         getEnv("API_KEY", ""),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
