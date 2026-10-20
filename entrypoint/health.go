package main

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// checkHealth implements both probes.
//
// Liveness ("health") only asserts that chronyd is up and has bound its
// sockets. It deliberately does not require synchronisation: chronyd refuses
// to answer clients until it is synchronised, so a stricter liveness probe
// would restart every replica during an upstream outage and make the outage
// worse.
//
// Readiness ("ready") asserts that the server actually serves usable time, so
// that a Service only sends clients to a replica that answers correctly.
func checkHealth(ready bool) error {
	c := loadConfig()

	if err := checkListening(c); err != nil {
		return err
	}

	if c.NTPPort == "0" {
		// Client-only mode: there is no NTP server to query, ask chronyd
		// through its command socket instead.
		if !ready {
			fmt.Println("alive: command socket present")
			return nil
		}
		return checkTracking(c)
	}

	reply, err := queryNTP(c)
	if err != nil {
		if ready {
			return err
		}
		// Not synchronised yet (or still refusing clients): alive all the same.
		fmt.Fprintf(os.Stderr, "chrony-entrypoint: not serving time yet: %v\n", err)
		fmt.Println("alive: sockets bound, time not served yet")
		return nil
	}

	if err := reply.validate(c, ready); err != nil {
		if ready {
			return err
		}
		fmt.Fprintf(os.Stderr, "chrony-entrypoint: degraded: %v\n", err)
		fmt.Println("alive: sockets bound, time degraded")
		return nil
	}

	if ready {
		if err := checkNTSKE(c); err != nil {
			return err
		}
	}
	fmt.Printf("ok: stratum %d, root distance %.6fs, leap %d\n",
		reply.stratum, reply.rootDistance(), reply.leap)
	return nil
}

// checkListening verifies that chronyd owns the NTP port, or, in client-only
// mode (port 0), that its command socket exists.
func checkListening(c *Config) error {
	if c.NTPPort == "0" {
		sock := filepath.Join(c.StateDir, "chronyd.sock")
		if !exists(sock) {
			return fmt.Errorf("command socket %s is missing", sock)
		}
		return nil
	}
	port, err := strconv.Atoi(c.NTPPort)
	if err != nil {
		return fmt.Errorf("NTP_PORT=%q is not a port number", c.NTPPort)
	}
	for _, f := range []string{"/proc/net/udp", "/proc/net/udp6"} {
		ok, err := procNetHasPort(f, port)
		if err != nil {
			continue
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("no socket bound to udp/%d", port)
}

func procNetHasPort(path string, port int) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	s.Scan() // header
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 {
			continue
		}
		_, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		p, err := strconv.ParseInt(hexPort, 16, 32)
		if err == nil && int(p) == port {
			return true, nil
		}
	}
	return false, s.Err()
}

// ntpReply is the subset of an NTP packet the probes look at.
type ntpReply struct {
	leap      uint8
	version   uint8
	mode      uint8
	stratum   uint8
	rootDelay float64
	rootDisp  float64
}

func (r ntpReply) rootDistance() float64 { return r.rootDelay/2 + r.rootDisp }

func (r ntpReply) validate(c *Config, ready bool) error {
	if r.mode != 4 {
		return fmt.Errorf("reply has mode %d, want 4 (server)", r.mode)
	}
	if r.version != 4 {
		return fmt.Errorf("reply has NTP version %d, want 4", r.version)
	}
	if r.leap == 3 {
		return fmt.Errorf("server reports leap=3 (clock not synchronised)")
	}
	if r.stratum == 0 || int(r.stratum) > c.HealthStratum {
		return fmt.Errorf("stratum %d outside 1..%d", r.stratum, c.HealthStratum)
	}
	if ready && r.rootDistance() > c.ReadyMaxRootDis {
		return fmt.Errorf("root distance %.6fs above READY_MAX_ROOT_DISTANCE=%.6fs",
			r.rootDistance(), c.ReadyMaxRootDis)
	}
	return nil
}

// queryNTP sends a real client request to the local server, which exercises
// the whole serving path: socket, access rules and source selection.
func queryNTP(c *Config) (ntpReply, error) {
	var lastErr error
	for _, host := range healthHosts(c) {
		r, err := queryNTPAt(c, host)
		if err == nil {
			return r, nil
		}
		lastErr = err
	}
	return ntpReply{}, lastErr
}

// healthHosts lists the loopback addresses to probe. Both families are tried
// by default so the probes work on IPv4-only, IPv6-only and dual-stack pods.
func healthHosts(c *Config) []string {
	switch {
	case c.HealthAddr != "":
		return []string{c.HealthAddr}
	case c.IPMode == "4":
		return []string{"127.0.0.1"}
	case c.IPMode == "6":
		return []string{"::1"}
	}
	return []string{"127.0.0.1", "::1"}
}

func queryNTPAt(c *Config, host string) (ntpReply, error) {
	var zero ntpReply

	addr := net.JoinHostPort(host, c.NTPPort)
	conn, err := net.DialTimeout("udp", addr, time.Duration(c.HealthTimeout*float64(time.Second)))
	if err != nil {
		return zero, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	req := make([]byte, 48)
	req[0] = 0<<6 | 4<<3 | 3 // leap 0, version 4, mode 3 (client)
	// Random transmit timestamp, echoed back by the server, so a stray packet
	// from somewhere else cannot satisfy the probe.
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return zero, err
	}
	copy(req[40:48], nonce)

	deadline := time.Now().Add(time.Duration(c.HealthTimeout * float64(time.Second)))
	if err := conn.SetDeadline(deadline); err != nil {
		return zero, err
	}
	if _, err := conn.Write(req); err != nil {
		return zero, fmt.Errorf("sending NTP request: %w", err)
	}

	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return zero, fmt.Errorf("no usable NTP reply from %s: %w", addr, err)
		}
		if n < 48 {
			continue
		}
		if string(buf[24:32]) != string(nonce) {
			continue // not the answer to our request
		}
		return ntpReply{
			leap:      buf[0] >> 6,
			version:   (buf[0] >> 3) & 0x7,
			mode:      buf[0] & 0x7,
			stratum:   buf[1],
			rootDelay: ntpShort(buf[4:8]),
			rootDisp:  ntpShort(buf[8:12]),
		}, nil
	}
}

// ntpShort decodes the 16.16 fixed point seconds used for delay/dispersion.
func ntpShort(b []byte) float64 {
	return float64(binary.BigEndian.Uint32(b)) / 65536
}

// checkNTSKE makes sure the NTS-KE listener accepts connections when this
// instance is configured as an NTS server.
func checkNTSKE(c *Config) error {
	if len(c.NTSCerts) == 0 {
		return nil
	}
	var lastErr error
	for _, host := range healthHosts(c) {
		addr := net.JoinHostPort(host, c.NTSPort)
		conn, err := net.DialTimeout("tcp", addr, time.Duration(c.HealthTimeout*float64(time.Second)))
		if err == nil {
			return conn.Close()
		}
		lastErr = fmt.Errorf("NTS-KE listener %s: %w", addr, err)
	}
	return lastErr
}

// checkTracking reads "chronyc -c tracking" over the Unix command socket. The
// CSV fields used are: 2 stratum, 10 root delay, 11 root dispersion, 13 leap.
func checkTracking(c *Config) error {
	sock := filepath.Join(c.StateDir, "chronyd.sock")
	out, err := exec.Command("/usr/bin/chronyc", "-h", sock, "-c", "tracking").Output()
	if err != nil {
		return fmt.Errorf("chronyc tracking: %w", err)
	}
	f := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(f) < 14 {
		return fmt.Errorf("unexpected chronyc tracking output %q", out)
	}
	if f[13] != "Normal" && f[13] != "Insert second" && f[13] != "Delete second" {
		return fmt.Errorf("leap status %q (not synchronised)", f[13])
	}
	stratum, _ := strconv.Atoi(f[2])
	delay, _ := strconv.ParseFloat(f[10], 64)
	disp, _ := strconv.ParseFloat(f[11], 64)
	r := ntpReply{version: 4, mode: 4, stratum: uint8(stratum), rootDelay: delay, rootDisp: disp}
	if err := r.validate(c, true); err != nil {
		return err
	}
	fmt.Printf("ok: stratum %d, root distance %.6fs (client only)\n", stratum, r.rootDistance())
	return nil
}
