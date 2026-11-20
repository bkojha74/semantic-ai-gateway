package config

import (
	"bufio"
	"os"
	"strings"
)

type Config struct {
	Port           string
	GeminiAPIKey   string
	MongoURI       string
	RedisAddr      string
	EmbeddingModel string
	DefaultModel   string
}

// Load reads configuration from environment variables and automatically
// populates any unset keys from a local .env file if present.
func Load() *Config {
	loadDotEnv(".env")

	return &Config{
		Port:           getEnv("PORT", "8099"),
		GeminiAPIKey:   getEnv("GEMINI_API_KEY", ""),
		MongoURI:       getEnv("MONGO_URI", "mongodb://localhost:27017/?directConnection=true"),
		RedisAddr:      getEnv("REDIS_ADDR", "localhost:6379"),
		EmbeddingModel: getEnv("EMBEDDING_MODEL", "gemini-embedding-001"),
		DefaultModel:   getEnv("DEFAULT_MODEL", "gemini-3.5-flash"),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

// loadDotEnv parses a standard KEY=VALUE .env file without external dependencies.
func loadDotEnv(filenames ...string) {
	for _, filename := range filenames {
		file, err := os.Open(filename)
		if err != nil {
			continue
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`)

			// Only set if not already set in active environment
			if os.Getenv(key) == "" {
				_ = os.Setenv(key, val)
			}
		}
	}
}
