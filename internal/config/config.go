// Package config loads api-log's runtime configuration from YAML and
// environment overrides. Layout follows ARCHITECTURE.md § 11.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/2nd1st/api-log/internal/plugin/builtin/pathfilter"
)

// Config is the parsed runtime config.
//
// All durations are stored as time.Duration. YAML uses *_seconds keys so the
// config file stays readable; we convert on load.
type Config struct {
	Proxy       ProxyConfig       `yaml:"proxy"`
	API         APIConfig         `yaml:"api"`
	Storage     StorageConfig     `yaml:"storage"`
	Timeouts    TimeoutsConfig    `yaml:"timeouts"`
	Shutdown    ShutdownConfig    `yaml:"shutdown"`
	Logging     LoggingConfig     `yaml:"logging"`
	Diagnostics DiagnosticsConfig `yaml:"diagnostics"`
	Plugins     PluginsConfig     `yaml:"plugins"`
	Media       MediaConfig       `yaml:"media"`
}

// MediaConfig governs per-trace media extraction (see phase-k-media-contract.md).
//
// SaveAttachments=true makes the writer-side extractor walk request/response
// bodies for named base64 fields (image_url.url, source.data, inlineData,
// b64_json) and persist each as a derived file under
// <data_dir>/<YYYY-MM-DD>/<keyhash>/media/<trace_id>/<idx>.<ext>. The JSONL
// remains the source of truth (Backend §6); these files are a fast-read
// cache + export-bundling target.
//
// Default is true (operator decision 2026-05-30). The runtime-overrides
// layer (internal/runtime) can flip this without a restart via
// PUT /api/config/media, and that override takes precedence over both
// yaml and env per contract § 6.
type MediaConfig struct {
	SaveAttachments bool `yaml:"save_attachments"`
}

// PluginsConfig holds per-plugin config subtrees for the Phase A
// observe-class plugin pipeline. The zero value disables every plugin
// (no plugins are constructed in main) — matching the documented
// "empty plugins block = current behavior" contract in
// uiux-research/plugin.md § 5.
//
// PHASE A SCAFFOLD ONLY: this struct is parsed but NOT yet read by
// main.go to construct a plugin.Registry. The wiring lands in
// Phase A.1 as its own commit. Until then, populating this block has
// no runtime effect — the JSONL output is identical with or without it.
type PluginsConfig struct {
	PathFilter pathfilter.Config `yaml:"path_filter"`
}

type ProxyConfig struct {
	Listen   string `yaml:"listen"`
	Upstream string `yaml:"upstream"`
}

type APIConfig struct {
	Listen string `yaml:"listen"`
}

type StorageConfig struct {
	DataDir          string `yaml:"data_dir"`
	MaxBodyBytes     int64  `yaml:"max_body_bytes"`
	CaptureChanSize  int    `yaml:"capture_chan_size"`
	WriterChanSize   int    `yaml:"writer_chan_size"`
}

type TimeoutsConfig struct {
	ReadHeaderSeconds      int `yaml:"read_header_seconds"`
	IdleSeconds           int `yaml:"idle_seconds"`
	StreamIdleSeconds     int `yaml:"stream_idle_seconds"`
	ReqBodyCaptureSeconds int `yaml:"req_body_capture_seconds"`

	// End-to-end traces this slow trigger a WARN log + slow_traces++.
	// 0 disables (no WARN). Default 30s.
	SlowTraceSeconds int `yaml:"slow_trace_seconds"`
}

// Diagnostics knobs — periodic background work that doesn't touch the
// trace path; safe to leave at defaults.
type DiagnosticsConfig struct {
	// Cadence for the counter-snapshot INFO line. 0 disables.
	// Default 60s; tight enough for prod incident timelines, loose
	// enough not to spam logs.
	SnapshotIntervalSeconds int `yaml:"snapshot_interval_seconds"`
}

type ShutdownConfig struct {
	GraceSeconds int `yaml:"grace_seconds"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
}

// Defaults returns a Config with the documented v0 defaults from
// ARCHITECTURE.md § 11.
func Defaults() Config {
	return Config{
		Proxy: ProxyConfig{
			Listen:   ":7861",
			Upstream: "http://localhost:7860",
		},
		API: APIConfig{
			Listen: ":7862",
		},
		Storage: StorageConfig{
			DataDir:         "./data",
			MaxBodyBytes:    32 << 20, // 32 MB
			CaptureChanSize: 64,
			WriterChanSize:  1024,
		},
		Timeouts: TimeoutsConfig{
			ReadHeaderSeconds:     10,
			IdleSeconds:           120,
			StreamIdleSeconds:     600,
			ReqBodyCaptureSeconds: 60,
			SlowTraceSeconds:      30,
		},
		Shutdown: ShutdownConfig{
			GraceSeconds: 30,
		},
		Logging: LoggingConfig{
			Level: "info",
		},
		Diagnostics: DiagnosticsConfig{
			SnapshotIntervalSeconds: 60,
		},
		Media: MediaConfig{
			// On by default per Phase K operator decision (2026-05-30).
			// Disable via APILOG_MEDIA_SAVE_ATTACHMENTS=false, the
			// yaml `media.save_attachments: false`, or runtime
			// PUT /api/config/media.
			SaveAttachments: true,
		},
	}
}

// Load reads the YAML file at path (empty path = defaults only), then
// applies APILOG_* environment overrides on top.
func Load(path string) (Config, error) {
	cfg := Defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
	}

	if err := applyEnv(&cfg); err != nil {
		return cfg, fmt.Errorf("apply env overrides: %w", err)
	}

	return cfg, nil
}

// envBindings maps APILOG_* names to setters. Keep flat; we want explicit
// rather than reflective so the supported override set is auditable.
type envBinding struct {
	name string
	set  func(*Config, string) error
}

func envInt(s string) (int, error) { return strconv.Atoi(strings.TrimSpace(s)) }

func envInt64(s string) (int64, error) { return strconv.ParseInt(strings.TrimSpace(s), 10, 64) }

// envBool parses the same forms strconv.ParseBool accepts ("1","t","T",
// "true","TRUE","True","0","f","F","false","FALSE","False"). We trim
// whitespace first to be forgiving of `FOO= true` in shell exports.
func envBool(s string) (bool, error) { return strconv.ParseBool(strings.TrimSpace(s)) }

func applyEnv(cfg *Config) error {
	bindings := []envBinding{
		{"APILOG_PROXY_LISTEN", func(c *Config, v string) error { c.Proxy.Listen = v; return nil }},
		{"APILOG_PROXY_UPSTREAM", func(c *Config, v string) error { c.Proxy.Upstream = v; return nil }},
		{"APILOG_API_LISTEN", func(c *Config, v string) error { c.API.Listen = v; return nil }},
		{"APILOG_STORAGE_DATA_DIR", func(c *Config, v string) error { c.Storage.DataDir = v; return nil }},
		{"APILOG_STORAGE_MAX_BODY_BYTES", func(c *Config, v string) error {
			n, err := envInt64(v)
			if err != nil {
				return err
			}
			c.Storage.MaxBodyBytes = n
			return nil
		}},
		{"APILOG_STORAGE_CAPTURE_CHAN_SIZE", func(c *Config, v string) error {
			n, err := envInt(v)
			if err != nil {
				return err
			}
			c.Storage.CaptureChanSize = n
			return nil
		}},
		{"APILOG_STORAGE_WRITER_CHAN_SIZE", func(c *Config, v string) error {
			n, err := envInt(v)
			if err != nil {
				return err
			}
			c.Storage.WriterChanSize = n
			return nil
		}},
		{"APILOG_TIMEOUTS_READ_HEADER_SECONDS", func(c *Config, v string) error {
			n, err := envInt(v)
			if err != nil {
				return err
			}
			c.Timeouts.ReadHeaderSeconds = n
			return nil
		}},
		{"APILOG_TIMEOUTS_IDLE_SECONDS", func(c *Config, v string) error {
			n, err := envInt(v)
			if err != nil {
				return err
			}
			c.Timeouts.IdleSeconds = n
			return nil
		}},
		{"APILOG_TIMEOUTS_STREAM_IDLE_SECONDS", func(c *Config, v string) error {
			n, err := envInt(v)
			if err != nil {
				return err
			}
			c.Timeouts.StreamIdleSeconds = n
			return nil
		}},
		{"APILOG_TIMEOUTS_REQ_BODY_CAPTURE_SECONDS", func(c *Config, v string) error {
			n, err := envInt(v)
			if err != nil {
				return err
			}
			c.Timeouts.ReqBodyCaptureSeconds = n
			return nil
		}},
		{"APILOG_TIMEOUTS_SLOW_TRACE_SECONDS", func(c *Config, v string) error {
			n, err := envInt(v)
			if err != nil {
				return err
			}
			c.Timeouts.SlowTraceSeconds = n
			return nil
		}},
		{"APILOG_DIAGNOSTICS_SNAPSHOT_INTERVAL_SECONDS", func(c *Config, v string) error {
			n, err := envInt(v)
			if err != nil {
				return err
			}
			c.Diagnostics.SnapshotIntervalSeconds = n
			return nil
		}},
		{"APILOG_SHUTDOWN_GRACE_SECONDS", func(c *Config, v string) error {
			n, err := envInt(v)
			if err != nil {
				return err
			}
			c.Shutdown.GraceSeconds = n
			return nil
		}},
		{"APILOG_LOGGING_LEVEL", func(c *Config, v string) error { c.Logging.Level = v; return nil }},
		{"APILOG_MEDIA_SAVE_ATTACHMENTS", func(c *Config, v string) error {
			b, err := envBool(v)
			if err != nil {
				return err
			}
			c.Media.SaveAttachments = b
			return nil
		}},
	}

	for _, b := range bindings {
		if v, ok := os.LookupEnv(b.name); ok {
			if err := b.set(cfg, v); err != nil {
				return fmt.Errorf("%s=%q: %w", b.name, v, err)
			}
		}
	}
	return nil
}

// Validate checks that the loaded configuration is internally consistent.
// Returns a wrapped error naming the offending field.
func (c Config) Validate() error {
	if c.Proxy.Listen == "" {
		return fmt.Errorf("proxy.listen is empty")
	}
	if c.Proxy.Upstream == "" {
		return fmt.Errorf("proxy.upstream is empty")
	}
	if c.API.Listen == "" {
		return fmt.Errorf("api.listen is empty")
	}
	if c.Storage.DataDir == "" {
		return fmt.Errorf("storage.data_dir is empty")
	}
	if c.Storage.MaxBodyBytes <= 0 {
		return fmt.Errorf("storage.max_body_bytes must be > 0, got %d", c.Storage.MaxBodyBytes)
	}
	if c.Storage.CaptureChanSize <= 0 {
		return fmt.Errorf("storage.capture_chan_size must be > 0, got %d", c.Storage.CaptureChanSize)
	}
	if c.Storage.WriterChanSize <= 0 {
		return fmt.Errorf("storage.writer_chan_size must be > 0, got %d", c.Storage.WriterChanSize)
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level must be one of debug|info|warn|error, got %q", c.Logging.Level)
	}
	return nil
}

// Duration helpers (config keeps seconds-as-int; callers want Duration).

func (c TimeoutsConfig) ReadHeader() time.Duration { return time.Duration(c.ReadHeaderSeconds) * time.Second }
func (c TimeoutsConfig) Idle() time.Duration        { return time.Duration(c.IdleSeconds) * time.Second }
func (c TimeoutsConfig) StreamIdle() time.Duration  { return time.Duration(c.StreamIdleSeconds) * time.Second }
func (c TimeoutsConfig) ReqBodyCapture() time.Duration {
	return time.Duration(c.ReqBodyCaptureSeconds) * time.Second
}
func (c TimeoutsConfig) SlowTrace() time.Duration {
	return time.Duration(c.SlowTraceSeconds) * time.Second
}

func (c ShutdownConfig) Grace() time.Duration { return time.Duration(c.GraceSeconds) * time.Second }

func (c DiagnosticsConfig) SnapshotInterval() time.Duration {
	return time.Duration(c.SnapshotIntervalSeconds) * time.Second
}
