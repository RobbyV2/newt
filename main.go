package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fosrl/newt/clients/permissions"
	"github.com/fosrl/newt/logger"
	newtpkg "github.com/fosrl/newt/newt"
)

var (
	newtVersion  = "version_replaceme"
	newtPlatform = ""
)

func main() {
	// Subcommand dispatch
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "auth-daemon":
			if len(os.Args) > 2 && os.Args[2] == "principals" {
				runPrincipalsCmd(os.Args[3:])
				return
			}
			fmt.Println("Error: auth-daemon subcommand requires 'principals' argument")
			fmt.Println()
			fmt.Println("Usage:")
			fmt.Println("  newt auth-daemon principals [options]")
			fmt.Println()
			return
		}
	}

	if isWindowsService() {
		runService("NewtWireguardService", false, os.Args[1:])
		return
	}

	if handleServiceCommand() {
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runNewtMain(ctx)
}

func runNewtMain(ctx context.Context) {
	logger.Init(nil)

	cfg := loadNewtConfig()

	if cfg.UseNativeMainInterface {
		if err := permissions.CheckNativeInterfacePermissions(); err != nil {
			logger.Fatal("Insufficient permissions for native main tunnel interface: %v", err)
		}
	}

	if err := validateTLSConfig(cfg); err != nil {
		logger.Fatal("TLS configuration error: %v", err)
	}

	logger.Debug("Endpoint: %v", cfg.Endpoint)
	logger.Debug("Log Level: %v", cfg.LogLevel)
	logger.Debug("Health Check Certificate Enforcement: %v", cfg.EnforceHealthcheckCert)
	if cfg.TLSClientCert != "" {
		logger.Debug("TLS Client Cert File: %v", cfg.TLSClientCert)
	}
	if cfg.TLSClientKey != "" {
		logger.Debug("TLS Client Key File: %v", cfg.TLSClientKey)
	}
	if len(cfg.TLSClientCAs) > 0 {
		logger.Debug("TLS CA Files: %v", cfg.TLSClientCAs)
	}
	if cfg.TLSPrivateKey != "" {
		logger.Debug("TLS PKCS12 File: %v", cfg.TLSPrivateKey)
	}
	if cfg.DNS != "" {
		logger.Debug("DNS: %v", cfg.DNS)
	}
	if cfg.MTU != 0 {
		logger.Debug("MTU: %v", cfg.MTU)
	}
	if cfg.UpdownScript != "" {
		logger.Debug("Up Down Script: %v", cfg.UpdownScript)
	}

	cfg.OnRestart = reexec

	n, err := newtpkg.Init(ctx, cfg)
	if err != nil {
		logger.Fatal("Failed to initialize newt: %v", err)
	}

	n.Start(ctx)
}

// runNewtMainWithArgs is used by the Windows service runner.
func runNewtMainWithArgs(ctx context.Context, args []string) {
	os.Args = append([]string{os.Args[0]}, args...)
	setupWindowsEventLog()
	runNewtMain(ctx)
}
