// chrony-entrypoint is a tiny PID 1 for a distroless chrony image.
//
// It renders /run/chrony/state/chrony.conf from the environment (or uses a
// mounted configuration as-is), seeds the NTS server keys when they are
// provided by a Secret, and then exec()s chronyd so that chronyd itself is
// PID 1 and receives SIGTERM directly.
//
// Subcommands:
//
//	run      render the configuration and exec chronyd (default)
//	config   render the configuration, validate it with "chronyd -p", print it
//	health   query the local NTP server, succeed if it answers
//	ready    like health, but also require a usable synchronised time
//	version  print the entrypoint and chronyd versions
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	// Under /run/chrony so that the host's AppArmor profile for
	// /usr/sbin/chronyd (Debian/Ubuntu), which also applies to unconfined
	// containers by path, allows it: "@{run}/chrony/{,**} rw" covers every
	// level, whereas "/var/lib/chrony/{,*} rw" covers a single one.
	stateDir = "/run/chrony/state"
	// chronyd compares the owner of the command socket directory with this
	// user (its compiled-in default user, "nonroot" in distroless).
	chronyUID, chronyGID = 65532, 65532
	chronydPath          = "/usr/sbin/chronyd"
)

var version = "dev"

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "run":
		fatal(run())
	case "config":
		fatal(printConfig())
	case "health":
		fatal(checkHealth(false))
	case "ready":
		fatal(checkHealth(true))
	case "version", "-v", "--version":
		fmt.Printf("chrony-entrypoint %s\n", version)
		out, err := exec.Command(chronydPath, "-v").CombinedOutput()
		fmt.Print(string(out))
		fatal(err)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

const usage = `usage: chrony-entrypoint [run|config|health|ready|version]

  run      render the configuration and exec chronyd (default)
  config   render the configuration, validate it, print it to stdout
  health   query the local NTP server, succeed if it answers
  ready    like health, but also require usable synchronised time
  version  print the entrypoint and chronyd versions

Configuration is read from the environment; see README.md.
`

func fatal(err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "chrony-entrypoint: %v\n", err)
	os.Exit(1)
}

// run prepares the state directory and replaces this process with chronyd.
func run() error {
	cfg := loadConfig()

	if err := prepareState(cfg); err != nil {
		return err
	}

	confPath, err := materialiseConfig(cfg)
	if err != nil {
		return err
	}
	if err := checkPrivilegedPorts(cfg); err != nil {
		return err
	}

	argv := chronydArgv(cfg, confPath)
	fmt.Fprintf(os.Stderr, "chrony-entrypoint: exec %s\n", strings.Join(argv, " "))
	return syscall.Exec(chronydPath, argv, os.Environ())
}

// printConfig renders the configuration and asks chronyd to parse it.
func printConfig() error {
	cfg := loadConfig()

	if err := prepareState(cfg); err != nil {
		return err
	}
	confPath, err := materialiseConfig(cfg)
	if err != nil {
		return err
	}

	out, err := exec.Command(chronydPath, "-p", "-f", confPath).CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		return fmt.Errorf("chronyd rejected the configuration: %w", err)
	}
	return nil
}

// prepareState makes sure the writable state directory exists and, when the
// NTS server keys are provided read-only (e.g. by a Secret), copies them into
// the state directory where chronyd expects to find them.
func prepareState(cfg *Config) error {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("state directory %s: %w", cfg.StateDir, err)
	}
	if err := unixWritable(cfg.StateDir); err != nil {
		if os.Geteuid() == 0 {
			return fmt.Errorf("state directory %s: %w (started as root without CAP_DAC_OVERRIDE: run as uid 65532 instead)", cfg.StateDir, err)
		}
		return fmt.Errorf("state directory %s: %w", cfg.StateDir, err)
	}
	if os.Geteuid() != 0 {
		if err := secureStateDir(cfg.StateDir); err != nil {
			return err
		}
	}

	// A mounted CHRONY_CONF may rely on the compiled-in pidfile. It can only
	// ever be stale (chronyd is PID 1 here) and would make chronyd refuse to
	// start, so remove it.
	if err := os.Remove(filepath.Join(cfg.StateDir, "chronyd.pid")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale pidfile: %w", err)
	}

	if cfg.NTSKeysFile == "" {
		return nil
	}
	data, err := os.ReadFile(cfg.NTSKeysFile)
	if err != nil {
		return fmt.Errorf("NTS server keys %s: %w", cfg.NTSKeysFile, err)
	}
	dst := filepath.Join(cfg.StateDir, "ntskeys")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("seeding %s: %w", dst, err)
	}
	fmt.Fprintf(os.Stderr, "chrony-entrypoint: seeded shared NTS server keys from %s\n", cfg.NTSKeysFile)
	return nil
}

// materialiseConfig returns the path of the configuration chronyd must read:
// either the one mounted by the operator, or the one rendered here.
func materialiseConfig(cfg *Config) (string, error) {
	if cfg.ConfigFile != "" {
		fmt.Fprintf(os.Stderr, "chrony-entrypoint: using mounted configuration %s (environment ignored)\n", cfg.ConfigFile)
		return cfg.ConfigFile, nil
	}

	conf, err := render(cfg)
	if err != nil {
		return "", err
	}
	path := filepath.Join(cfg.StateDir, "chrony.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

func chronydArgv(cfg *Config, confPath string) []string {
	// -r reloads the sample histories saved in dumpdir on the previous exit,
	// so a restarted replica becomes ready again in seconds, not minutes.
	argv := []string{chronydPath, "-f", confPath, "-d", "-U", "-r"}

	if !cfg.ClockControl {
		// Do not touch the system clock: no CAP_SYS_TIME needed, the node
		// clock is left to the node and chronyd serves corrected time.
		argv = append(argv, "-x")
	}
	if cfg.SeccompLevel != 0 {
		argv = append(argv, "-F", fmt.Sprint(cfg.SeccompLevel))
	}
	if cfg.LogLevel != "" {
		argv = append(argv, "-L", cfg.LogLevel)
	}
	if cfg.IPMode == "4" {
		argv = append(argv, "-4")
	}
	if cfg.IPMode == "6" {
		argv = append(argv, "-6")
	}
	argv = append(argv, cfg.ExtraArgs...)
	return argv
}

// unixWritable reports whether the current process can create files in dir.
func unixWritable(dir string) error {
	probe := filepath.Join(dir, ".writable")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("not writable by uid %d: %w", os.Geteuid(), err)
	}
	f.Close()
	return os.Remove(probe)
}

// checkPrivilegedPorts fails early when chronyd would not be able to bind its
// ports. chronyd itself only logs "Could not open NTP socket" and keeps
// running as a client, which hides the misconfiguration.
func checkPrivilegedPorts(cfg *Config) error {
	if os.Geteuid() == 0 || hasCapability(capNetBindService) {
		return nil
	}
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start")
	if err != nil {
		return nil // cannot tell, let chronyd try
	}
	start, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return nil
	}
	ports := map[string]string{"NTP_PORT": cfg.NTPPort}
	if len(cfg.NTSCerts) > 0 {
		ports["NTS_PORT"] = cfg.NTSPort
	}
	for name, v := range ports {
		port, err := strconv.Atoi(v)
		if err != nil || port == 0 || port >= start {
			continue
		}
		return fmt.Errorf("%s=%d is below net.ipv4.ip_unprivileged_port_start=%d and uid %d has no CAP_NET_BIND_SERVICE: "+
			"set the pod sysctl net.ipv4.ip_unprivileged_port_start=%d, or use %s>=%d behind a Service",
			name, port, start, os.Geteuid(), port, name, start)
	}
	return nil
}

const capNetBindService = 10

// hasCapability reads the effective capability set from /proc/self/status.
func hasCapability(bit uint) bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
			mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			return err == nil && mask&(1<<bit) != 0
		}
	}
	return false
}

// secureStateDir gives the state directory the permissions chronyd demands
// for its command socket: no access for others, owned by its user and group.
// A volume such as a Kubernetes emptyDir is world-writable (0777); chronyd
// would then log "Wrong permissions" and silently disable the socket, leaving
// chronyc and any exporter blind. Mount the volume on the parent directory
// and let this function lock down the subdirectory.
func secureStateDir(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("state directory %s: chmod 0700: %w", dir, err)
	}
	// With fsGroup the directory inherits another group through the setgid
	// bit of the volume; take our own group back (we are a member of both).
	if err := os.Chown(dir, -1, os.Getegid()); err != nil {
		return fmt.Errorf("state directory %s: chgrp %d: %w", dir, os.Getegid(), err)
	}
	if os.Geteuid() != chronyUID || os.Getegid() != chronyGID {
		warn("running as %d:%d, not %d:%d: chronyd will refuse its command socket "+
			"(chronyc, exporters and the client-only readiness check will not work)",
			os.Geteuid(), os.Getegid(), chronyUID, chronyGID)
	}
	return nil
}
