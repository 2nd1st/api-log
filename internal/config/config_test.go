package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsValidate(t *testing.T) {
	c := Defaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config failed validation: %v", err)
	}
}

func TestLoadDefaultsWhenPathEmpty(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if c.Proxy.Listen != ":7861" {
		t.Errorf("Proxy.Listen = %q, want :7861", c.Proxy.Listen)
	}
	if c.Storage.MaxBodyBytes != 32<<20 {
		t.Errorf("Storage.MaxBodyBytes = %d, want %d", c.Storage.MaxBodyBytes, 32<<20)
	}
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-log.yaml")
	if err := os.WriteFile(path, []byte(`
proxy:
  listen: ":9999"
  upstream: "http://example:7860"
storage:
  max_body_bytes: 1048576
logging:
  level: "debug"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Proxy.Listen != ":9999" {
		t.Errorf("Proxy.Listen = %q", c.Proxy.Listen)
	}
	if c.Proxy.Upstream != "http://example:7860" {
		t.Errorf("Proxy.Upstream = %q", c.Proxy.Upstream)
	}
	if c.Storage.MaxBodyBytes != 1048576 {
		t.Errorf("Storage.MaxBodyBytes = %d", c.Storage.MaxBodyBytes)
	}
	if c.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q", c.Logging.Level)
	}
	// Defaults preserved for unspecified fields.
	if c.API.Listen != ":7862" {
		t.Errorf("API.Listen = %q, want default :7862", c.API.Listen)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("APILOG_PROXY_UPSTREAM", "http://env-override:7860")
	t.Setenv("APILOG_STORAGE_MAX_BODY_BYTES", "67108864")
	t.Setenv("APILOG_LOGGING_LEVEL", "warn")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Proxy.Upstream != "http://env-override:7860" {
		t.Errorf("Proxy.Upstream = %q", c.Proxy.Upstream)
	}
	if c.Storage.MaxBodyBytes != 67108864 {
		t.Errorf("Storage.MaxBodyBytes = %d", c.Storage.MaxBodyBytes)
	}
	if c.Logging.Level != "warn" {
		t.Errorf("Logging.Level = %q", c.Logging.Level)
	}
}

func TestEnvOverrideInvalid(t *testing.T) {
	t.Setenv("APILOG_STORAGE_MAX_BODY_BYTES", "not-a-number")
	if _, err := Load(""); err == nil {
		t.Fatal("Load() with bad env should error, got nil")
	}
}

func TestValidateRejectsBadLevel(t *testing.T) {
	c := Defaults()
	c.Logging.Level = "verbose"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() should reject unknown level")
	}
}

func TestValidateRejectsZeroChan(t *testing.T) {
	c := Defaults()
	c.Storage.CaptureChanSize = 0
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() should reject zero capture chan size")
	}
}

func TestMediaDefaultsOn(t *testing.T) {
	// Phase K operator decision 2026-05-30: extraction ON by default.
	c := Defaults()
	if !c.Media.SaveAttachments {
		t.Errorf("Media.SaveAttachments default = false, want true")
	}
}

func TestMediaYAMLOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-log.yaml")
	if err := os.WriteFile(path, []byte(`
media:
  save_attachments: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Media.SaveAttachments {
		t.Errorf("Media.SaveAttachments = true, want false (yaml override)")
	}
}

func TestMediaEnvOverride(t *testing.T) {
	t.Setenv("APILOG_MEDIA_SAVE_ATTACHMENTS", "false")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Media.SaveAttachments {
		t.Errorf("Media.SaveAttachments = true, want false (env override)")
	}
}

func TestMediaEnvOverrideInvalid(t *testing.T) {
	t.Setenv("APILOG_MEDIA_SAVE_ATTACHMENTS", "notabool")
	if _, err := Load(""); err == nil {
		t.Fatal("Load() with bad media env should error, got nil")
	}
}

