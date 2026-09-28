// Package config loads runtime configuration from the environment.
//
// Precedence: real environment variables always win over values in .env, so a
// container or systemd unit can override the file without editing it.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// DefaultUserAgent is a browser-like string, and that is not cosmetic.
//
// Many Chinese government sites reject non-browser User-Agents outright.
// 中国人事考试网 answers 405 to both "Go-http-client/1.1" and to an honest
// "Mozilla/5.0 (compatible; exams-bot/...)" string, and only serves content to
// a full browser UA; 教育部 returns a stripped-down page without the data
// unless it sees one. Override with HTTP_USER_AGENT if you would rather
// identify yourself and accept that those sources will fail.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// Config is the fully resolved application configuration.
type Config struct {
	Addr         string
	DatabaseURL  string
	Location     *time.Location
	LocationName string

	// Crawling
	CrawlCron        string
	CrawlOnStart     bool
	CrawlTimeout     time.Duration
	CrawlConcurrency int
	CrawlMaxItems    int
	CrawlMaxDetails  int

	// Outbound HTTP for collectors
	UserAgent string
	HTTPProxy string

	LogLevel string

	// PluginsDir holds Lua collector scripts, loaded at startup. Missing
	// directory is fine: Lua sources are optional.
	PluginsDir string

	// Notifications
	NotifyEnabled  bool
	NotifyOnUpdate bool
	NotifyTimeout  time.Duration

	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string
	NotifyTo []string

	WebhookURL    string
	WebhookSecret string
	// WebhookFlavor selects the payload shape: generic, dingtalk, wecom or
	// feishu. A generic JSON body is silently rejected by all three chat
	// platforms, so this is not cosmetic.
	WebhookFlavor string
}

// RequireDB reports whether a database connection is configured. Commands that
// need one call this; `probe` deliberately does not, so a broken selector can
// be debugged without a working database.
func (c *Config) RequireDB() error {
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required for this command (see .env.example)")
	}
	return nil
}

// Load reads configuration from the environment, optionally seeded by a .env
// file in the working directory. A missing .env is not an error.
func Load() (*Config, error) {
	_ = godotenv.Load(".env")

	loc, name := loadLocation()

	cfg := &Config{
		Addr:         env("APP_ADDR", ":8080"),
		DatabaseURL:  env("DATABASE_URL", ""),
		Location:     loc,
		LocationName: name,

		CrawlCron:        env("CRAWL_CRON", "0 */2 * * *"),
		CrawlOnStart:     envBool("CRAWL_ON_START", true),
		CrawlTimeout:     envDuration("CRAWL_TIMEOUT", 60*time.Second),
		CrawlConcurrency: envInt("CRAWL_CONCURRENCY", 4),
		CrawlMaxItems:    envInt("CRAWL_MAX_ITEMS", 500),
		CrawlMaxDetails:  envInt("CRAWL_MAX_DETAILS", 40),

		UserAgent: env("HTTP_USER_AGENT", DefaultUserAgent),
		HTTPProxy: env("HTTP_PROXY", ""),

		LogLevel:   env("LOG_LEVEL", "info"),
		PluginsDir: env("PLUGINS_DIR", "./plugins"),

		NotifyEnabled:  envBool("NOTIFY_ENABLED", false),
		NotifyOnUpdate: envBool("NOTIFY_ON_UPDATE", false),
		NotifyTimeout:  envDuration("NOTIFY_TIMEOUT", 20*time.Second),

		SMTPHost: env("SMTP_HOST", ""),
		SMTPPort: envInt("SMTP_PORT", 587),
		SMTPUser: env("SMTP_USER", ""),
		SMTPPass: env("SMTP_PASS", ""),
		SMTPFrom: env("SMTP_FROM", ""),
		NotifyTo: envList("NOTIFY_EMAIL_TO"),

		WebhookURL:    env("WEBHOOK_URL", ""),
		WebhookSecret: env("WEBHOOK_SECRET", ""),
		WebhookFlavor: env("WEBHOOK_FLAVOR", "generic"),
	}

	if cfg.CrawlConcurrency < 1 {
		return nil, fmt.Errorf("CRAWL_CONCURRENCY must be >= 1, got %d", cfg.CrawlConcurrency)
	}
	if cfg.CrawlMaxItems < 1 {
		return nil, fmt.Errorf("CRAWL_MAX_ITEMS must be >= 1, got %d", cfg.CrawlMaxItems)
	}
	if cfg.SMTPFrom == "" {
		cfg.SMTPFrom = cfg.SMTPUser
	}
	if err := cfg.validateNotify(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validateNotify rejects NOTIFY_ENABLED=true with nothing to notify through,
// which would otherwise fail silently on every crawl.
func (c *Config) validateNotify() error {
	if !c.NotifyEnabled {
		return nil
	}
	var have []string
	if c.SMTPHost != "" && len(c.NotifyTo) > 0 {
		have = append(have, "email")
	}
	if c.WebhookURL != "" {
		have = append(have, "webhook")
	}
	if len(have) == 0 {
		return errors.New("NOTIFY_ENABLED=true but no channel is configured: " +
			"set SMTP_HOST+NOTIFY_EMAIL_TO for email, and/or WEBHOOK_URL for webhook")
	}
	return nil
}

// loadLocation resolves TZ. It falls back to UTC rather than failing, since a
// wrong timezone is a cosmetic problem but a failed startup is not.
func loadLocation() (*time.Location, string) {
	name := env("TZ", "Asia/Shanghai")
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC, "UTC"
	}
	return loc, name
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}

// envList parses a comma-separated list, dropping blank entries.
func envList(key string) []string {
	v := env(key, "")
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
