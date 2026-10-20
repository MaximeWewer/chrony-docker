package main

import (
	"strings"
	"testing"
)

func baseConfig() *Config {
	return &Config{
		StateDir:      "/run/chrony/state",
		SourceOptions: "iburst",
		NTPPort:       "123",
		NTSPort:       "4460",
	}
}

func mustRender(t *testing.T, c *Config) string {
	t.Helper()
	out, err := render(c)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNoImplicitAllowAll(t *testing.T) {
	c := baseConfig()
	c.Allow = []string{"10.0.0.0/8"}
	out := mustRender(t, c)
	if strings.Contains(out, "allow all") || strings.Contains(out, "allow 0.0.0.0/0") {
		t.Fatalf("unexpected open access:\n%s", out)
	}
	for _, want := range []string{"allow 127.0.0.1", "allow ::1", "allow 10.0.0.0/8", "cmdport 0"} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRateLimitOffByDefault(t *testing.T) {
	out := mustRender(t, baseConfig())
	if strings.Contains(out, "ratelimit") {
		t.Fatalf("ratelimit must be opt-in (SNAT collapses clients):\n%s", out)
	}
	c := baseConfig()
	c.RateLimit = "on"
	if !strings.Contains(mustRender(t, c), "ratelimit interval 3 burst 8 leak 2\n") {
		t.Fatal("RATELIMIT=on not expanded")
	}
}

func TestSourcesAndNTS(t *testing.T) {
	c := baseConfig()
	c.NTSClient = true
	c.Pools = []string{"pool.example"}
	c.Servers = []string{"ntp1.example", "ntp2.example minpoll 4 nts"}
	out := mustRender(t, c)
	for _, want := range []string{
		"pool pool.example iburst nts\n",
		"server ntp1.example iburst nts\n",
		"server ntp2.example minpoll 4 nts\n",
		"authselectmode require\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestMixedSourcesKeepDefaultAuthSelect(t *testing.T) {
	c := baseConfig()
	c.NTSClient = true
	c.Servers = []string{"gps.local minpoll 0"}
	if strings.Contains(mustRender(t, c), "authselectmode") {
		t.Fatal("authselectmode require must not be forced on unauthenticated sources")
	}
}

func TestNTSServerSharedKeys(t *testing.T) {
	c := baseConfig()
	c.NTSCerts = []string{"/c.crt"}
	c.NTSKeys = []string{"/c.key"}
	c.NTSKeysFile = "/etc/chrony/nts/ntskeys"
	c.NTSNTPServer = []string{"ntp.example.com"}
	out := mustRender(t, c)
	for _, want := range []string{"ntsservercert /c.crt\n", "ntsserverkey /c.key\n", "ntsrotate 0\n", "ntsntpserver ntp.example.com\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	c.NTSKeys = nil
	if _, err := render(c); err == nil {
		t.Fatal("unbalanced cert/key lists must be rejected")
	}
}

func TestListKeepsPerSourceOptions(t *testing.T) {
	t.Setenv("NTP_SERVERS", "a.example, b.example iburst prefer\nc.example")
	got := list("NTP_SERVERS")
	if len(got) != 3 || got[1] != "b.example iburst prefer" {
		t.Fatalf("got %q", got)
	}
}

func TestPidfileDisabled(t *testing.T) {
	out := mustRender(t, baseConfig())
	if !strings.Contains(out, "pidfile /\n") || strings.Contains(out, ".pid") {
		t.Fatalf("pidfile must be disabled (stale PID 1 blocks restarts):\n%s", out)
	}
}
