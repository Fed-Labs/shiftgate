package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"shift.dev/shift/internal/agent"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/observability"
	shiftruntime "shift.dev/shift/internal/runtime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "shift-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	// The agent re-executes itself with this verb inside a private mount
	// namespace when a restore needs bind mounts. It is not a user-facing
	// command: it applies the mounts and then execs CRIU in that namespace.
	if len(os.Args) > 1 && os.Args[1] == checkpoint.MountNamespaceCommand {
		return checkpoint.RunMountNamespace(os.Args[2:])
	}
	// And with this verb to launch every workload: the shim installs the
	// io_uring seccomp filter and then execs the workload's command. Also not
	// user-facing.
	if len(os.Args) > 1 && os.Args[1] == shiftruntime.WorkloadExecCommand {
		return shiftruntime.RunWorkloadExec(os.Args[2:])
	}
	var configPath string
	var stateDir string
	var listen string
	var remoteListen string
	var certificate string
	var privateKey string
	var clientCA string
	var peerCA string
	var logLevel string
	var insecureDevelopment bool
	var showVersion bool
	var printDefault bool
	flag.StringVar(&configPath, "config", "", "path to agent JSON configuration")
	flag.StringVar(&stateDir, "state-dir", "", "agent state directory")
	flag.StringVar(&listen, "listen", "", "local listener, usually unix:///run/shift/agent.sock")
	flag.StringVar(&remoteListen, "remote-listen", "", "peer listener, for example tcp://0.0.0.0:8443")
	flag.StringVar(&certificate, "tls-cert", "", "TLS certificate PEM")
	flag.StringVar(&privateKey, "tls-key", "", "TLS private key PEM")
	flag.StringVar(&clientCA, "client-ca", "", "CA bundle for authenticated peer clients")
	flag.StringVar(&peerCA, "peer-ca", "", "CA bundle used to authenticate destination agents")
	flag.StringVar(&logLevel, "log-level", "", "debug, info, warn, or error")
	flag.BoolVar(&insecureDevelopment, "insecure-development", false, "enable local-only development relaxations")
	flag.BoolVar(&showVersion, "version", false, "print version")
	flag.BoolVar(&printDefault, "print-default-config", false, "print default JSON configuration")
	flag.Parse()
	if showVersion {
		fmt.Println("shift-agent", config.Version)
		return nil
	}
	if printDefault {
		return json.NewEncoder(os.Stdout).Encode(config.DefaultAgent())
	}
	configuration, err := config.LoadAgent(configPath)
	if err != nil {
		return err
	}
	if stateDir != "" {
		configuration.SetStateDir(stateDir)
	}
	if listen != "" {
		configuration.Listen = listen
	}
	if remoteListen != "" {
		configuration.RemoteListen = remoteListen
	}
	if certificate != "" {
		configuration.TLS.CertificateFile = certificate
	}
	if privateKey != "" {
		configuration.TLS.PrivateKeyFile = privateKey
	}
	if clientCA != "" {
		configuration.TLS.ClientCAFile = clientCA
	}
	if peerCA != "" {
		configuration.TLS.PeerCAFile = peerCA
	}
	if logLevel != "" {
		configuration.LogLevel = logLevel
	}
	if insecureDevelopment {
		configuration.InsecureDevelopment = true
	}
	if err := configuration.Validate(); err != nil {
		return err
	}
	logger := observability.NewLogger(configuration.LogLevel)
	service, err := agent.Open(configuration, logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return service.Run(ctx)
}
