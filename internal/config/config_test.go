package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "token = 'test-token'\nnetwork_id = 'vpc-id'\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Token: "test-token", APIURL: DefaultAPIURL, Namespace: "timeweb",
		NetworkID: "vpc-id", RequestTimeoutSeconds: 60,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("loaded configuration differs from defaults: got %+v, want %+v", cfg, want)
	}
	if got := cfg.RequestTimeout(); got != time.Minute {
		t.Errorf("RequestTimeout() = %s, want 1m", got)
	}
}

func TestLoadExplicitValues(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
token = " test-token "
api_url = "https://api.example.test/api/v1/"
namespace = "GARM_pool-1.prod"
availability_zone = "msk-1"
project_id = 42
ssh_key_ids = [100, 101]
network_id = "existing-vpc"
network_mode = "no_nat"
request_timeout_seconds = 120
`))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Token: "test-token", APIURL: "https://api.example.test/api/v1/",
		Namespace: "GARM_pool-1.prod", AvailabilityZone: "msk-1", ProjectID: 42,
		SSHKeyIDs: []int64{100, 101}, NetworkID: "existing-vpc", NetworkMode: "no_nat",
		RequestTimeoutSeconds: 120,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("loaded configuration differs from explicit values: got %+v, want %+v", cfg, want)
	}
	if cfg.RequestTimeout() != 2*time.Minute {
		t.Errorf("RequestTimeout() = %s, want 2m", cfg.RequestTimeout())
	}
}

func TestLoadTokenFile(t *testing.T) {
	for _, relative := range []bool{true, false} {
		name := "absolute"
		if relative {
			name = "relative to config directory"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			credentialsDir := filepath.Join(dir, "credentials")
			if err := os.Mkdir(credentialsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			tokenPath := filepath.Join(credentialsDir, "token")
			if err := os.WriteFile(tokenPath, []byte("test-file-token\r\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			configuredPath := tokenPath
			if relative {
				configuredPath = filepath.Join("credentials", "token")
			}
			configPath := filepath.Join(dir, "provider.toml")
			if err := os.WriteFile(configPath, []byte("token_file = '"+configuredPath+"'\nnetwork_id = 'vpc-id'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Token != "test-file-token" || cfg.TokenFile != configuredPath {
				t.Error("token_file was not resolved or its configured path was lost")
			}
			if err := cfg.Validate(); err != nil {
				t.Errorf("loaded token_file configuration is invalid: %v", err)
			}
		})
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	const secret = "secret-that-must-not-appear-in-errors"
	tests := []struct {
		name string
		toml string
		want string
	}{
		{"missing credential", "network_id = 'vpc'", "exactly one"},
		{"two credentials", "token = '" + secret + "'\ntoken_file = 'missing'\nnetwork_id = 'vpc'", "exactly one"},
		{"empty token and token file", "token = ''\ntoken_file = 'missing'\nnetwork_id = 'vpc'", "exactly one"},
		{"empty token file", "token_file = ''\nnetwork_id = 'vpc'", "non-empty path"},
		{"blank token file", "token_file = '   '\nnetwork_id = 'vpc'", "non-empty path"},
		{"empty token", "token = ''\nnetwork_id = 'vpc'", "non-empty Timeweb token"},
		{"blank token", "token = '   '\nnetwork_id = 'vpc'", "non-empty Timeweb token"},
		{"internal token whitespace", "token = '" + secret + " extra'\nnetwork_id = 'vpc'", "without whitespace"},
		{"malformed TOML", "token = \"" + secret + "\nnetwork_id = 'vpc'", "invalid TOML"},
		{"wrong type", "token = '" + secret + "'\nnetwork_id = 'vpc'\nproject_id = '" + secret + "'", "invalid TOML"},
		{"unknown key", "token = '" + secret + "'\nnetwork_id = 'vpc'\n" + secret + " = 1", "unknown fields"},
		{"unknown nested table", "token = '" + secret + "'\nnetwork_id = 'vpc'\n[extra]\nvalue = 1", "unknown fields"},
		{"missing network", "token = '" + secret + "'", "network_id"},
		{"explicit zero timeout", "token = '" + secret + "'\nnetwork_id = 'vpc'\nrequest_timeout_seconds = 0", "request_timeout_seconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.toml))
			if err == nil {
				t.Fatal("Load succeeded with invalid configuration")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Error("configuration error exposed secret contents")
			}
			if !reflect.DeepEqual(cfg, Config{}) {
				t.Error("Load returned a partial configuration after failure")
			}
		})
	}
}

func TestLoadTokenFileErrors(t *testing.T) {
	for _, contents := range []string{"", " \r\n\t", "test-token\nanother-token"} {
		t.Run("invalid contents "+strings.ReplaceAll(contents, "\n", "newline"), func(t *testing.T) {
			configPath := writeConfig(t, "token_file = 'token'\nnetwork_id = 'vpc'\n")
			if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), "token"), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "Timeweb token") {
				t.Errorf("Load() error = %v, want invalid token error", err)
			}
		})
	}
	t.Run("missing token file", func(t *testing.T) {
		_, err := Load(writeConfig(t, "token_file = 'missing'\nnetwork_id = 'vpc'\n"))
		if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "read token_file") {
			t.Errorf("Load() error = %v, want token_file not found", err)
		}
	})
	t.Run("missing config file", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
		if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "read provider configuration") {
			t.Errorf("Load() error = %v, want configuration not found", err)
		}
	})
}

func TestValidate(t *testing.T) {
	valid := Config{
		Token: "test-token", APIURL: DefaultAPIURL, Namespace: "timeweb",
		NetworkID: "vpc-id", NetworkMode: "snat", RequestTimeoutSeconds: 60,
	}
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"empty token", func(c *Config) { c.Token = "" }},
		{"two credentials", func(c *Config) { c.TokenFile = "token" }},
		{"unresolved token file", func(c *Config) { c.Token, c.TokenFile = "", "token" }},
		{"token unicode whitespace", func(c *Config) { c.Token = "test\u00a0token" }},
		{"token control character", func(c *Config) { c.Token = "test\x00token" }},
		{"empty namespace", func(c *Config) { c.Namespace = "" }},
		{"namespace starts with hyphen", func(c *Config) { c.Namespace = "-pool" }},
		{"namespace whitespace", func(c *Config) { c.Namespace = "my pool" }},
		{"namespace too long", func(c *Config) { c.Namespace = strings.Repeat("a", 65) }},
		{"zero timeout", func(c *Config) { c.RequestTimeoutSeconds = 0 }},
		{"negative timeout", func(c *Config) { c.RequestTimeoutSeconds = -1 }},
		{"timeout exceeds limit", func(c *Config) { c.RequestTimeoutSeconds = 601 }},
		{"negative project", func(c *Config) { c.ProjectID = -1 }},
		{"zero SSH key", func(c *Config) { c.SSHKeyIDs = []int64{42, 0} }},
		{"negative SSH key", func(c *Config) { c.SSHKeyIDs = []int64{-1} }},
		{"missing VPC", func(c *Config) { c.NetworkID = "" }},
		{"VPC whitespace", func(c *Config) { c.NetworkID = " vpc" }},
		{"unknown network mode", func(c *Config) { c.NetworkMode = "automatic" }},
		{"public IP allocating network mode", func(c *Config) { c.NetworkMode = "dnat_and_snat" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.change(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("Validate succeeded with invalid configuration")
			}
		})
	}
	for _, mode := range []string{"", "snat", "no_nat"} {
		t.Run("valid network mode "+mode, func(t *testing.T) {
			cfg := valid
			cfg.NetworkMode = mode
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateAPIURL(t *testing.T) {
	tests := []struct {
		url   string
		valid bool
	}{
		{DefaultAPIURL, true},
		{"https://proxy.example.test/timeweb/", true},
		{"http://127.0.0.1:8080/api/v1", true},
		{"http://127.10.20.30/api/v1", true},
		{"http://[::1]:8080/api/v1", true},
		{"http://localhost:8080", true},
		{"http://LOCALHOST:8080", true},
		{"", false},
		{"/api/v1", false},
		{"https://:443/api/v1", false},
		{"http://api.timeweb.cloud/api/v1", false},
		{"http://localhost.example.test", false},
		{"http://192.168.1.10", false},
		{"ftp://127.0.0.1", false},
		{"https://user:password@example.test", false},
		{"https://example.test?token=secret", false},
		{"https://example.test?", false},
		{"https://example.test#fragment", false},
		{"https://%not-a-host", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			cfg := Config{
				Token: "test-token", APIURL: tt.url, Namespace: "timeweb",
				NetworkID: "vpc", NetworkMode: "snat", RequestTimeoutSeconds: 60,
			}
			if err := cfg.Validate(); (err == nil) != tt.valid {
				t.Errorf("Validate() error = %v, want valid=%v", err, tt.valid)
			}
		})
	}
}
