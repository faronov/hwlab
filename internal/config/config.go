// Package config resolves hwlab directories and settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Version is the hwlab version string (overridden at build time via -ldflags).
var Version = "0.1.0-dev"

// Config holds process-wide settings.
type Config struct {
	// Home is the hwlab home directory (default ~/.hwlab, or $HWLAB_HOME).
	Home string `yaml:"-"`
	// SessionsDir is where sessions are stored (default <home>/sessions).
	SessionsDir string `yaml:"sessions_dir"`
	// PluginPaths are extra plugin directories.
	PluginPaths []string `yaml:"plugin_paths"`
	// Verbose enables debug logging.
	Verbose bool `yaml:"-"`
}

// Load reads $HWLAB_HOME/config.yaml (optional) and applies defaults.
func Load() (Config, error) {
	var c Config
	c.Home = os.Getenv("HWLAB_HOME")
	if c.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return c, fmt.Errorf("locate home directory: %w", err)
		}
		c.Home = filepath.Join(h, ".hwlab")
	}
	b, err := os.ReadFile(filepath.Join(c.Home, "config.yaml"))
	switch {
	case err == nil:
		if err := yaml.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse %s/config.yaml: %w", c.Home, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return c, err
	}
	if env := os.Getenv("HWLAB_SESSIONS"); env != "" {
		c.SessionsDir = env
	}
	if c.SessionsDir == "" {
		c.SessionsDir = filepath.Join(c.Home, "sessions")
	}
	return c, nil
}
