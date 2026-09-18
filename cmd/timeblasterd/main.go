// Command timeblasterd is the Timeblaster daemon: alarm clock, hardware
// controller, television controller and companion-app server.
//
// It runs unprivileged under systemd as the timeblaster user. Privileged
// networking is delegated to timeblaster-wifi over a unix socket; this process
// can neither change the network nor run an arbitrary command.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bradsheets/timeblaster/internal/app"
	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/logging"
)

// version is set at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
var version = "dev"

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "timeblasterd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", config.DefaultPath, "path to the configuration file")
		checkConfig = flag.Bool("check-config", false, "validate the configuration and exit")
		printConfig = flag.Bool("print-config", false, "print the effective configuration and exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
		logLevel    = flag.String("log-level", "", "override logging.level (debug, info, warn, error)")
		logTime     = flag.Bool("log-time", false, "include timestamps in log output (journald adds its own)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("timeblasterd", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		// Configuration errors are reported before the logger exists, so they go
		// straight to stderr where systemd will capture them.
		return fmt.Errorf("configuration: %w", err)
	}
	if *logLevel != "" {
		cfg.Logging.Level = *logLevel
	}

	if *checkConfig {
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("configuration is invalid: %w", err)
		}
		fmt.Printf("configuration at %s is valid\n", *configPath)
		return nil
	}
	if *printConfig {
		return printEffectiveConfig(cfg)
	}

	log := logging.New(logging.Options{
		Level:       cfg.Logging.Level,
		Format:      cfg.Logging.Format,
		IncludeTime: *logTime,
	})

	// SIGTERM is what systemd sends on `systemctl stop` and on a restart; SIGINT
	// is what a developer sends with ctrl-C. Both must unwind cleanly, because
	// leaving an alarm ringing or an mpv process orphaned is unacceptable.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	application, err := app.New(cfg, log, version, app.Deps{})
	if err != nil {
		return err
	}

	// A second signal during a slow shutdown exits immediately, so a wedged
	// subsystem cannot make the service un-stoppable.
	go func() {
		<-ctx.Done()
		hard := make(chan os.Signal, 1)
		signal.Notify(hard, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-hard:
			log.Warn("second signal received; exiting immediately")
			os.Exit(1)
		case <-time.After(30 * time.Second):
			log.Error("shutdown took longer than 30 seconds; exiting")
			os.Exit(1)
		}
	}()

	err = application.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// printEffectiveConfig writes the configuration actually in use, defaults
// included. It is the quickest way to answer "why is it using that audio
// device" on a device you are logged into over SSH.
func printEffectiveConfig(cfg config.Config) error {
	enc := tomlEncoder(os.Stdout)
	return enc.Encode(cfg)
}
