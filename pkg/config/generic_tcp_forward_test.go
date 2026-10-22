package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenericTCPForwardConfigPrecedence(t *testing.T) {
	t.Setenv("ENABLE_GENERIC_TCP_FORWARD", "true")
	var conf Config
	loadConfigFromEnv(&conf)
	if !conf.EnableGenericTCPForward {
		t.Fatal("environment switch not loaded")
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("ENABLE_GENERIC_TCP_FORWARD: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loadConfigFromFile(path, &conf)
	if conf.EnableGenericTCPForward {
		t.Fatal("file must override only its explicitly configured environment values")
	}
}
