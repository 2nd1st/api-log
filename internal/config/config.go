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

	"github.com/2nd1st/api-log/internal/plugin/builtin/capturefilter"
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
	Viewer      ViewerConfig      `yaml:"viewer"`
}

// ViewerConfig governs the optional hosted-viewer feature
// (`/viewer/*`). The backend fetches the viewer's release `dist.zip`
// once at startup, sha-verifies it against a backend-pinned constant
// (cmd/api-log/viewer_pins.go), extracts to a local cache, and serves
// it. Defaults are ON, pinned to the backend's source-baked version
// + SHA.
//
// Empty Repo / Version / Sha256 fields are filled in from the
// backend-pinned constants in main.go. An empty CacheDir is resolved
// to `<DataDir>/viewer-cache`. An empty PublicPath defaults to
// `/viewer`. LocalPath overrides the fetch entirely (offline mode).
//
// Operator-override safety: if the operator overrides Repo or Version
// without supplying a matching Sha256 (and not using LocalPath), the
// viewerhost rejects the override — the binary stays up, but
// `/viewer/` returns 503 with a logged error. We never serve an
// unverified asset.
type ViewerConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Repo       string `yaml:"repo"`
	Version    string `yaml:"version"`
	Sha256     string `yaml:"sha256"`
	LocalPath  string `yaml:"local_path"`
	CacheDir   string `yaml:"cache_dir"`
	PublicPath string `yaml:"public_path"`

	// ReleasesAPIBase overrides the dist.zip fetch endpoint. Empty
	// means "use https://api.github.com" (the default). Internal
	// artifact stores (Gitea, Forgejo) speak the same release-API
	// shape and slot in via this field — e.g. setting it to
	// "http://gitea.homelab.lan/api/v1" with a matching `Repo` =
	// "<gitea-owner>/<repo>" reroutes the fetch.
	ReleasesAPIBase string `yaml:"releases_api_base"`

	// ReleasesAuthToken is sent on the metadata + asset requests as
	// `Authorization: token <value>`. Required for private Gitea
	// repos. Sensitive — source from env (APILOG_VIEWER_RELEASES_AUTH_TOKEN)
	// or a secrets file rather than committing to repo-tracked YAML.
	ReleasesAuthToken string `yaml:"releases_auth_token"`
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
// Default is true. The runtime-overrides layer can flip this without a
// restart via PUT /api/config/media, and that override takes precedence over
// both yaml and env.
type MediaConfig struct {
	SaveAttachments bool `yaml:"save_attachments"`
}

// PluginsConfig holds observe-class plugin config. The zero value disables
// every observe-class plugin, so an empty plugins block preserves default
// recording behavior.
//
// path_filter (existing): observer-class drop at finalize. Trace IS
// captured to the data dir; only the SQLite mirror is suppressed.
// Operators who configured this pre-v0.1.2 keep their semantics.
//
// capture_filter (v0.1.2+): pre-write drop. Matches BEFORE the proxy
// handler starts a trace; no JSONL line + no SQLite row + no media
// extraction + no body tee. Use for high-volume polling endpoints
// that shouldn't consume disk at all. Opt-in; identical pattern
// syntax so operators can copy a string between the two blocks.
type PluginsConfig struct {
	PathFilter    pathfilter.Config    `yaml:"path_filter"`
	CaptureFilter capturefilter.Config `yaml:"capture_filter"`
}

type ProxyConfig struct {
	Listen   string `yaml:"listen"`
	Upstream string `yaml:"upstream"`
}

type APIConfig struct {
	Listen string `yaml:"listen"`

	// ExportByteHardCap is the byte ceiling on /api/export before
	// `?bytes_all=1` is required to bypass. Measured in source JSONL
	// bytes (sum of file sizes for matched groups, one os.Stat per
	// group). 0 = unlimited; default 2 GiB.
	//
	// Independent of the row cap. See internal/api/export.go's
	// defaultExportByteHardCap docstring for the rationale; this YAML
	// field overrides that default. Env var:
	// APILOG_API_EXPORT_BYTE_HARDCAP.
	ExportByteHardCap int64 `yaml:"export_byte_hardcap"`
}

type StorageConfig struct {
	DataDir         string `yaml:"data_dir"`
	MaxBodyBytes    int64  `yaml:"max_body_bytes"`
	CaptureChanSize int    `yaml:"capture_chan_size"`
	WriterChanSize  int    `yaml:"writer_chan_size"`
}

type TimeoutsConfig struct {
	ReadHeaderSeconds     int `yaml:"read_header_seconds"`
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
		Viewer: ViewerConfig{
			// On by default. Operator opts out via
			// APILOG_VIEWER_ENABLED=false. Empty Repo / Version /
			// Sha256 are filled from cmd/api-log/viewer_pins.go in
			// main.go so the config-package layer stays free of the
			// release-ceremony constants. Empty CacheDir is resolved
			// to <DataDir>/viewer-cache at startup.
			Enabled:    true,
			Repo:       "",
			Version:    "",
			Sha256:     "",
			LocalPath:  "",
			CacheDir:   "",
			PublicPath: "/viewer",
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
		{"APILOG_API_EXPORT_BYTE_HARDCAP", func(c *Config, v string) error {
			n, err := envInt64(v)
			if err != nil {
				return err
			}
			c.API.ExportByteHardCap = n
			return nil
		}},
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
		{"APILOG_VIEWER_ENABLED", func(c *Config, v string) error {
			b, err := envBool(v)
			if err != nil {
				return err
			}
			c.Viewer.Enabled = b
			return nil
		}},
		{"APILOG_VIEWER_REPO", func(c *Config, v string) error { c.Viewer.Repo = v; return nil }},
		{"APILOG_VIEWER_VERSION", func(c *Config, v string) error { c.Viewer.Version = v; return nil }},
		{"APILOG_VIEWER_SHA256", func(c *Config, v string) error { c.Viewer.Sha256 = v; return nil }},
		{"APILOG_VIEWER_LOCAL_PATH", func(c *Config, v string) error { c.Viewer.LocalPath = v; return nil }},
		{"APILOG_VIEWER_CACHE_DIR", func(c *Config, v string) error { c.Viewer.CacheDir = v; return nil }},
		{"APILOG_VIEWER_PUBLIC_PATH", func(c *Config, v string) error { c.Viewer.PublicPath = v; return nil }},
		{"APILOG_VIEWER_RELEASES_API_BASE", func(c *Config, v string) error { c.Viewer.ReleasesAPIBase = v; return nil }},
		{"APILOG_VIEWER_RELEASES_AUTH_TOKEN", func(c *Config, v string) error { c.Viewer.ReleasesAuthToken = v; return nil }},
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

func (c TimeoutsConfig) ReadHeader() time.Duration {
	return time.Duration(c.ReadHeaderSeconds) * time.Second
}
func (c TimeoutsConfig) Idle() time.Duration { return time.Duration(c.IdleSeconds) * time.Second }
func (c TimeoutsConfig) StreamIdle() time.Duration {
	return time.Duration(c.StreamIdleSeconds) * time.Second
}
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
