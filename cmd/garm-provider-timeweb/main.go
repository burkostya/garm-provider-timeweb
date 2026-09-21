// Command garm-provider-timeweb implements the GARM external provider protocol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/burkostya/garm-provider-timeweb/internal/config"
	"github.com/burkostya/garm-provider-timeweb/internal/provider"
	"github.com/cloudbase/garm-provider-common/execution/common"
	execution "github.com/cloudbase/garm-provider-common/execution/v0.1.0"
)

// Release builds populate these values with -ldflags -X main.<name>=<value>.
var (
	version = "devel"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(common.ResolveErrorToExitCode(err))
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("garm-provider-timeweb", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version and build metadata")
	checkConfig := flags.String("check-config", "", "validate a TOML configuration without calling Timeweb")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	checkConfigSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "check-config" {
			checkConfigSet = true
		}
	})
	if *showVersion && checkConfigSet {
		return fmt.Errorf("--version and --check-config cannot be combined")
	}
	if *showVersion {
		_, err := fmt.Fprintf(stdout, "garm-provider-timeweb %s (commit %s, built %s)\n", version, commit, date)
		return err
	}
	if checkConfigSet {
		if *checkConfig == "" {
			return fmt.Errorf("--check-config requires a configuration file path")
		}
		if _, err := config.Load(*checkConfig); err != nil {
			return err
		}
		_, err := fmt.Fprintln(stdout, "Configuration is valid.")
		return err
	}

	// Upstream uses a leading "v". The unset version is the legacy v0.1.0
	// contract, which is also what GARM selects when its config omits it.
	if v := os.Getenv("GARM_INTERFACE_VERSION"); v != "" && v != common.Version010 {
		return fmt.Errorf("unsupported GARM interface version: this release supports v0.1.0")
	}
	// Version discovery does not require credentials or a working configuration.
	if os.Getenv("GARM_COMMAND") == string(common.GetVersionCommand) {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}

	env, err := execution.GetEnvironment()
	if err != nil {
		return err
	}
	if env.Command == common.CreateInstanceCommand && env.BootstrapParams.PoolID != env.PoolID {
		return fmt.Errorf("bootstrap pool_id does not match GARM_POOL_ID")
	}
	cfg, err := config.Load(env.ProviderConfigFile)
	if err != nil {
		return err
	}
	prov, err := provider.New(cfg, env.ControllerID, version)
	if err != nil {
		return err
	}
	result, err := env.Run(ctx, prov)
	if err != nil {
		return err
	}
	if result != "" {
		_, err = fmt.Fprintln(stdout, result)
	}
	return err
}
