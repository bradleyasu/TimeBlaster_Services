// Command timeblaster-wifi is Timeblaster's privileged network helper.
//
// It runs as root and is the only component that may change the network: it
// owns every nmcli invocation and serves the captive-portal page while setup
// mode is active. The main daemon runs unprivileged and reaches it through a
// small, validated RPC on a unix socket, so a bug in the alarm clock or the web
// API cannot reconfigure networking or run a command.
//
// Wi-Fi setup has exactly one entry path: holding the dedicated button for five
// seconds. This helper never raises an access point on its own.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/logging"
	"github.com/bradsheets/timeblaster/internal/system"
	"github.com/bradsheets/timeblaster/internal/wifi"
)

var version = "dev"

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "timeblaster-wifi: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath   = flag.String("config", config.DefaultPath, "path to the configuration file")
		showVersion  = flag.Bool("version", false, "print the version and exit")
		logLevel     = flag.String("log-level", "", "override logging.level")
		allowNonRoot = flag.Bool("allow-non-root", false,
			"run without root, for development; network changes will fail")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("timeblaster-wifi", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if *logLevel != "" {
		cfg.Logging.Level = *logLevel
	}

	log := logging.New(logging.Options{Level: cfg.Logging.Level, Format: cfg.Logging.Format})

	if os.Geteuid() != 0 && !*allowNonRoot {
		return errors.New("timeblaster-wifi must run as root " +
			"(pass -allow-non-root for development)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("timeblaster-wifi starting",
		"version", version,
		"socket", cfg.WiFi.HelperSocket,
		"interface", cfg.WiFi.Interface,
		"setup_ssid", cfg.WiFi.SetupSSID,
		"setup_secured", cfg.WiFi.SetupPassphrase != "")

	server := wifi.NewServer(cfg.WiFi, system.ExecRunner{}, system.RealClock{}, log)

	err = server.Listen(ctx)
	if errors.Is(err, context.Canceled) {
		log.Info("timeblaster-wifi stopped")
		return nil
	}
	return err
}
