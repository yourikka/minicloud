package main

import (
	"bytes"
	"testing"
	"time"
)

func TestParseConfigFlagsOverrideEnvironment(t *testing.T) {
	t.Parallel()
	environment := map[string]string{
		"MINICLOUD_DATA_DIR":      "environment-data",
		"MINICLOUD_LISTEN":        "127.0.0.1:9000",
		"MINICLOUD_VALIDATOR":     "environment-validator",
		"MINICLOUD_SYNC_INTERVAL": "2s",
		"MINICLOUD_TLS_CERT":      "environment-cert.pem",
		"MINICLOUD_TLS_KEY":       "environment-key.pem",
	}
	config, err := parseConfig([]string{
		"-data-dir", "flag-data",
		"-listen", "127.0.0.1:9100",
		"-validator", "flag-validator",
		"-sync-interval", "500ms",
		"-tls-cert", "flag-cert.pem",
		"-tls-key", "flag-key.pem",
	}, func(name string) string { return environment[name] }, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if config.DataRoot != "flag-data" || config.ValidatorCommand != "flag-validator" ||
		config.SyncInterval != 500*time.Millisecond || config.HTTP.Address != "127.0.0.1:9100" ||
		config.HTTP.TLS == nil || config.HTTP.TLS.Certificate != "flag-cert.pem" ||
		config.HTTP.TLS.PrivateKey != "flag-key.pem" {
		t.Fatalf("parseConfig() = %+v", config)
	}
}

func TestParseConfigRejectsInvalidEnvironmentAndArguments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		args   []string
		getenv func(string) string
	}{
		{
			name: "invalid duration environment",
			getenv: func(name string) string {
				if name == "MINICLOUD_SYNC_INTERVAL" {
					return "later"
				}
				return ""
			},
		},
		{name: "positional argument", args: []string{"unexpected"}, getenv: func(string) string { return "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseConfig(test.args, test.getenv, &bytes.Buffer{}); err == nil {
				t.Fatal("parseConfig() accepted invalid input")
			}
		})
	}
}
