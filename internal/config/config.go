// Package config loads the provider's TOML configuration and credentials.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
)

// DefaultAPIURL is the Timeweb Cloud API endpoint.
const DefaultAPIURL = "https://api.timeweb.cloud/api/v1"

var namespacePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// Config describes one provider namespace. Tokens stay on the GARM controller.
// Load resolves TokenFile into Token before returning the configuration.
type Config struct {
	Token                 string  `toml:"token"`
	TokenFile             string  `toml:"token_file"`
	APIURL                string  `toml:"api_url"`
	Namespace             string  `toml:"namespace"`
	AvailabilityZone      string  `toml:"availability_zone"`
	ProjectID             int64   `toml:"project_id"`
	SSHKeyIDs             []int64 `toml:"ssh_key_ids"`
	NetworkID             string  `toml:"network_id"`
	NetworkMode           string  `toml:"network_mode"` // Empty preserves the existing network's NAT setup.
	RequestTimeoutSeconds int     `toml:"request_timeout_seconds"`

	tokenFileResolved bool
}

// RequestTimeout returns the configured timeout for each API request.
func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
}

// Load applies defaults, rejects unknown TOML keys and validates the result.
// Exactly one of token or token_file must appear in the configuration.
// A relative token_file is resolved against the configuration file's directory.
// Decoder errors deliberately omit TOML values, which may contain credentials.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read provider configuration: %w", err)
	}
	cfg := Config{
		APIURL:                DefaultAPIURL,
		Namespace:             "timeweb",
		RequestTimeoutSeconds: 60,
	}
	meta, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("decode provider configuration: invalid TOML")
	}
	if len(meta.Undecoded()) != 0 {
		return Config{}, fmt.Errorf("provider configuration contains unknown fields")
	}
	if meta.IsDefined("token") == meta.IsDefined("token_file") {
		return Config{}, fmt.Errorf("configure exactly one of token or token_file")
	}
	if meta.IsDefined("token_file") {
		if strings.TrimSpace(cfg.TokenFile) == "" {
			return Config{}, fmt.Errorf("token_file must be a non-empty path")
		}
		p := cfg.TokenFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(path), p)
		}
		token, err := os.ReadFile(p)
		if err != nil {
			return Config{}, fmt.Errorf("read token_file: %w", err)
		}
		cfg.Token = string(token)
		cfg.tokenFileResolved = true
	}
	cfg.Token = strings.TrimSpace(cfg.Token)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks a resolved configuration without making network requests.
// Use Load to apply defaults and resolve credentials from a token_file.
func (c Config) Validate() error {
	if c.TokenFile != "" && !c.tokenFileResolved {
		if c.Token != "" {
			return fmt.Errorf("configure exactly one of token or token_file")
		}
		return fmt.Errorf("use Load to resolve token_file before validating")
	}
	if c.Token == "" || strings.ContainsFunc(c.Token, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return fmt.Errorf("a non-empty Timeweb token without whitespace or control characters is required")
	}
	u, err := url.Parse(c.APIURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("api_url must be an absolute HTTPS URL without credentials, query or fragment")
	}
	loopback := strings.EqualFold(u.Hostname(), "localhost")
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return fmt.Errorf("api_url must use HTTPS (HTTP is allowed only for loopback addresses)")
	}
	if !namespacePattern.MatchString(c.Namespace) {
		return fmt.Errorf("namespace must be 1-64 letters, digits, underscores, dots or hyphens, starting with a letter or digit")
	}
	if c.RequestTimeoutSeconds < 1 || c.RequestTimeoutSeconds > 600 {
		return fmt.Errorf("request_timeout_seconds must be between 1 and 600")
	}
	if c.ProjectID < 0 {
		return fmt.Errorf("project_id must be zero (omitted) or a positive ID")
	}
	for _, id := range c.SSHKeyIDs {
		if id <= 0 {
			return fmt.Errorf("ssh_key_ids must contain positive IDs")
		}
	}
	if c.NetworkID == "" || strings.ContainsFunc(c.NetworkID, unicode.IsSpace) {
		return fmt.Errorf("network_id is required and must identify an existing VPC with outbound connectivity")
	}
	switch c.NetworkMode {
	case "", "snat", "no_nat":
	default:
		return fmt.Errorf("network_mode must be empty (preserve existing setup), snat or no_nat")
	}
	return nil
}
