package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yourikka/minicloud/internal/gatewayhttp"
	"github.com/yourikka/minicloud/internal/localcore"
)

const defaultProcessCloseTimeout = 30 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stderr))
}

func run(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	stderr io.Writer,
) int {
	config, err := parseConfig(args, getenv, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "minicloud: %v\n", err)
		return 2
	}
	config.OnError = func(err error) {
		fmt.Fprintf(stderr, "minicloud: convergence: %v\n", err)
	}
	runtime, err := localcore.New(ctx, config)
	if err != nil {
		fmt.Fprintf(stderr, "minicloud: startup: %v\n", err)
		return 1
	}
	runErr := runtime.Run(ctx)
	closeContext, cancel := context.WithTimeout(context.Background(), defaultProcessCloseTimeout)
	closeErr := runtime.Close(closeContext)
	cancel()
	if err := errors.Join(runErr, closeErr); err != nil {
		fmt.Fprintf(stderr, "minicloud: shutdown: %v\n", err)
		return 1
	}
	return 0
}

func parseConfig(
	args []string,
	getenv func(string) string,
	stderr io.Writer,
) (localcore.Config, error) {
	if getenv == nil {
		return localcore.Config{}, errors.New("environment reader is required")
	}
	if stderr == nil {
		return localcore.Config{}, errors.New("error output is required")
	}
	syncInterval, err := environmentDuration(getenv, "MINICLOUD_SYNC_INTERVAL", localcore.DefaultSyncInterval)
	if err != nil {
		return localcore.Config{}, err
	}
	flags := flag.NewFlagSet("minicloud", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataRoot := flags.String("data-dir", environment(getenv, "MINICLOUD_DATA_DIR", ".minicloud"), "local data directory")
	address := flags.String("listen", environment(getenv, "MINICLOUD_LISTEN", gatewayhttp.DefaultAddress), "HTTP listen address")
	validatorCommand := flags.String(
		"validator",
		environment(getenv, "MINICLOUD_VALIDATOR", "minicloud-validator"),
		"validator executable",
	)
	tlsCertificate := flags.String("tls-cert", environment(getenv, "MINICLOUD_TLS_CERT", ""), "TLS certificate chain")
	tlsPrivateKey := flags.String("tls-key", environment(getenv, "MINICLOUD_TLS_KEY", ""), "TLS private key")
	managementAddress := flags.String(
		"management-listen",
		environment(getenv, "MINICLOUD_MANAGEMENT_LISTEN", ""),
		"management HTTP listen address (management stays disabled without a token)",
	)
	managementTokenFile := flags.String(
		"management-token-file",
		environment(getenv, "MINICLOUD_MANAGEMENT_TOKEN_FILE", ""),
		"file containing the static management token",
	)
	managementSubject := flags.String(
		"management-subject",
		environment(getenv, "MINICLOUD_MANAGEMENT_SUBJECT", ""),
		"stable management principal recorded by operations",
	)
	managementTLSCertificate := flags.String(
		"management-tls-cert",
		environment(getenv, "MINICLOUD_MANAGEMENT_TLS_CERT", ""),
		"management TLS certificate chain",
	)
	managementTLSPrivateKey := flags.String(
		"management-tls-key",
		environment(getenv, "MINICLOUD_MANAGEMENT_TLS_KEY", ""),
		"management TLS private key",
	)
	flags.DurationVar(&syncInterval, "sync-interval", syncInterval, "local convergence interval")
	if err := flags.Parse(args); err != nil {
		return localcore.Config{}, err
	}
	if flags.NArg() != 0 {
		return localcore.Config{}, errors.New("positional arguments are not supported")
	}
	var tlsFiles *gatewayhttp.TLSFiles
	if *tlsCertificate != "" || *tlsPrivateKey != "" {
		tlsFiles = &gatewayhttp.TLSFiles{Certificate: *tlsCertificate, PrivateKey: *tlsPrivateKey}
	}
	management, err := managementConfig(
		getenv,
		*managementAddress,
		*managementTokenFile,
		*managementSubject,
		*managementTLSCertificate,
		*managementTLSPrivateKey,
	)
	if err != nil {
		return localcore.Config{}, err
	}
	return localcore.Config{
		DataRoot:         *dataRoot,
		ValidatorCommand: *validatorCommand,
		SyncInterval:     syncInterval,
		HTTP: gatewayhttp.ServerConfig{
			Address: *address,
			TLS:     tlsFiles,
		},
		Management: management,
	}, nil
}

// managementConfig assembles the optional management boundary. The token comes
// from MINICLOUD_MANAGEMENT_TOKEN or a token file, never from a CLI argument,
// so it cannot leak through the process argument list.
func managementConfig(
	getenv func(string) string,
	address string,
	tokenFile string,
	subject string,
	tlsCertificate string,
	tlsPrivateKey string,
) (localcore.ManagementConfig, error) {
	token := getenv("MINICLOUD_MANAGEMENT_TOKEN")
	if tokenFile != "" {
		if token != "" {
			return localcore.ManagementConfig{}, errors.New(
				"MINICLOUD_MANAGEMENT_TOKEN and -management-token-file are mutually exclusive",
			)
		}
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return localcore.ManagementConfig{}, fmt.Errorf("reading management token file: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		if address != "" || subject != "" {
			return localcore.ManagementConfig{}, errors.New(
				"management listen address requires MINICLOUD_MANAGEMENT_TOKEN or -management-token-file",
			)
		}
		return localcore.ManagementConfig{}, nil
	}
	var tlsFiles *gatewayhttp.TLSFiles
	if tlsCertificate != "" || tlsPrivateKey != "" {
		tlsFiles = &gatewayhttp.TLSFiles{Certificate: tlsCertificate, PrivateKey: tlsPrivateKey}
	}
	return localcore.ManagementConfig{
		HTTP: gatewayhttp.ServerConfig{
			Address: address,
			TLS:     tlsFiles,
		},
		Token:   token,
		Subject: subject,
	}, nil
}

func environment(getenv func(string) string, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}
	return fallback
}

func environmentDuration(
	getenv func(string) string,
	name string,
	fallback time.Duration,
) (time.Duration, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", name, err)
	}
	return duration, nil
}
