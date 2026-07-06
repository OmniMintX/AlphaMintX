package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
)

// TestParseLiveOMS pins the startup guards (live-oms-and-reconciler.md
// §Config): paper is the default, any unknown mode/env refuses to start,
// credentials are required iff mode=live, and prod demands the exact ack
// literal — and secret values never appear in error text.
func TestParseLiveOMS(t *testing.T) {
	const key, secret = "test-api-key-value", "test-api-secret-value"
	cases := []struct {
		name    string
		mode    string
		env     string
		key     string
		secret  string
		ack     string
		tuning  string
		wantNil bool
		wantErr bool
		wantEnv exchange.Env
	}{
		{name: "default is paper", wantNil: true},
		{name: "explicit paper", mode: "paper", wantNil: true},
		{name: "paper ignores missing keys", mode: "paper", wantNil: true},
		{name: "unknown mode refused", mode: "LIVE", wantErr: true},
		{name: "live testnet default env", mode: "live", key: key, secret: secret, wantEnv: exchange.EnvTestnet},
		{name: "live explicit testnet", mode: "live", env: "testnet", key: key, secret: secret, wantEnv: exchange.EnvTestnet},
		{name: "unknown env refused", mode: "live", env: "mainnet", key: key, secret: secret, wantErr: true},
		{name: "live requires api key", mode: "live", secret: secret, wantErr: true},
		{name: "live requires api secret", mode: "live", key: key, wantErr: true},
		{name: "prod requires ack", mode: "live", env: "prod", key: key, secret: secret, wantErr: true},
		{name: "prod refuses wrong ack", mode: "live", env: "prod", key: key, secret: secret,
			ack: "i-understand-this-trades-real-funds", wantErr: true},
		{name: "prod with exact ack", mode: "live", env: "prod", key: key, secret: secret,
			ack: prodAckLiteral, wantEnv: exchange.EnvProd},
		{name: "bad tuning refused", mode: "live", key: key, secret: secret,
			tuning: `{"bogus":1}`, wantErr: true},
		{name: "tuning applies", mode: "live", key: key, secret: secret,
			tuning: `{"recv_window_ms":9000}`, wantEnv: exchange.EnvTestnet},
		{name: "nonpositive stall threshold refused", mode: "live", key: key, secret: secret,
			tuning: `{"safety_effect_stall_seconds":0}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseLiveOMS(tc.mode, tc.env, tc.key, tc.secret, tc.ack, tc.tuning)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				for _, s := range []string{key, secret} {
					if strings.Contains(err.Error(), s) {
						t.Fatalf("error text leaks a credential: %q", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLiveOMS: %v", err)
			}
			if tc.wantNil {
				if c != nil {
					t.Fatalf("config = %+v, want nil (paper)", c)
				}
				return
			}
			if c == nil || c.env != tc.wantEnv || c.apiKey != tc.key || c.apiSecret != tc.secret {
				t.Fatalf("config = %+v, want env %s with credentials", c, tc.wantEnv)
			}
			if tc.tuning != "" && c.tuning.RecvWindowMS != 9000 {
				t.Fatalf("tuning.RecvWindowMS = %d, want 9000", c.tuning.RecvWindowMS)
			}
		})
	}
}

// TestParseBreakerIntervals pins the breaker-monitor knobs
// (safety-wiring.md §Config): ACTIVE defaults to 5 s within [1, 10], IDLE
// to 60 s within [ACTIVE, 600]; out of bounds or non-integer refuses to
// start (fail closed).
func TestParseBreakerIntervals(t *testing.T) {
	cases := []struct {
		name       string
		active     string
		idle       string
		wantActive time.Duration
		wantIdle   time.Duration
		wantErr    bool
	}{
		{name: "defaults", wantActive: 5 * time.Second, wantIdle: 60 * time.Second},
		{name: "explicit values", active: "2", idle: "120",
			wantActive: 2 * time.Second, wantIdle: 120 * time.Second},
		{name: "active low bound", active: "1", wantActive: time.Second, wantIdle: 60 * time.Second},
		{name: "active high bound", active: "10", idle: "10",
			wantActive: 10 * time.Second, wantIdle: 10 * time.Second},
		{name: "idle equals active", active: "5", idle: "5",
			wantActive: 5 * time.Second, wantIdle: 5 * time.Second},
		{name: "idle high bound", idle: "600",
			wantActive: 5 * time.Second, wantIdle: 600 * time.Second},
		{name: "active zero refused", active: "0", wantErr: true},
		{name: "active above 10 refused", active: "11", wantErr: true},
		{name: "active non-integer refused", active: "5s", wantErr: true},
		{name: "idle below active refused", active: "5", idle: "4", wantErr: true},
		{name: "idle above 600 refused", idle: "601", wantErr: true},
		{name: "idle non-integer refused", idle: "sixty", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			active, idle, err := parseBreakerIntervals(tc.active, tc.idle)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBreakerIntervals: %v", err)
			}
			if active != tc.wantActive || idle != tc.wantIdle {
				t.Fatalf("intervals = %s/%s, want %s/%s", active, idle, tc.wantActive, tc.wantIdle)
			}
		})
	}
}

// TestValidateVenuePairing: prod trading refuses testnet market-data
// endpoint overrides; testnet trading pairs with anything.
func TestValidateVenuePairing(t *testing.T) {
	if err := validateVenuePairing(exchange.EnvProd, "", ""); err != nil {
		t.Fatalf("prod with prod market data: %v", err)
	}
	if err := validateVenuePairing(exchange.EnvProd, "https://data-api.binance.vision", ""); err != nil {
		t.Fatalf("prod with a prod mirror: %v", err)
	}
	if err := validateVenuePairing(exchange.EnvProd, "https://testnet.binance.vision", ""); err == nil {
		t.Fatal("prod with testnet REST market data: expected refusal")
	}
	if err := validateVenuePairing(exchange.EnvProd, "", "wss://stream.testnet.binance.vision"); err == nil {
		t.Fatal("prod with testnet WS market data: expected refusal")
	}
	if err := validateVenuePairing(exchange.EnvTestnet, "https://testnet.binance.vision", ""); err != nil {
		t.Fatalf("testnet pairing: %v", err)
	}
}

// TestParseWatchdogDisabled pins the escape hatch (watchdog.md §Config):
// "1"/"true" disable; anything else, including unset, enables.
func TestParseWatchdogDisabled(t *testing.T) {
	for v, want := range map[string]bool{
		"1": true, "true": true,
		"": false, "0": false, "false": false, "TRUE": false, "yes": false,
	} {
		if got := parseWatchdogDisabled(v); got != want {
			t.Errorf("parseWatchdogDisabled(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestParseBackupConfig pins the OB-8 fail-fast startup validation
// (ops-backup.md): dir "" disables the surface (nil config), the dir must
// exist and be a writable directory, and retain/interval are optional
// ints >= 0 where 0/unset disables each.
func TestParseBackupConfig(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name         string
		dir          string
		retain       string
		interval     string
		wantNil      bool
		wantErr      string
		wantRetain   int
		wantInterval time.Duration
	}{
		{name: "unset dir disables", dir: "", retain: "5", interval: "6", wantNil: true},
		{name: "dir only", dir: dir},
		{name: "full config", dir: dir, retain: "5", interval: "3",
			wantRetain: 5, wantInterval: 3 * time.Hour},
		{name: "explicit zeros are unset", dir: dir, retain: "0", interval: "0"},
		{name: "missing dir", dir: filepath.Join(dir, "absent"), wantErr: "must exist"},
		{name: "dir is a file", dir: writeTempFile(t, dir), wantErr: "not a directory"},
		{name: "negative retain", dir: dir, retain: "-1", wantErr: "CONTROLPLANE_BACKUP_RETAIN"},
		{name: "garbage retain", dir: dir, retain: "many", wantErr: "CONTROLPLANE_BACKUP_RETAIN"},
		{name: "negative interval", dir: dir, interval: "-1", wantErr: "CONTROLPLANE_BACKUP_INTERVAL_HOURS"},
		{name: "garbage interval", dir: dir, interval: "daily", wantErr: "CONTROLPLANE_BACKUP_INTERVAL_HOURS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseBackupConfig(tc.dir, tc.retain, tc.interval)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBackupConfig: %v", err)
			}
			if tc.wantNil {
				if cfg != nil {
					t.Fatalf("cfg = %+v, want nil (surface disabled)", cfg)
				}
				return
			}
			if cfg == nil || cfg.dir != tc.dir || cfg.retain != tc.wantRetain || cfg.interval != tc.wantInterval {
				t.Errorf("cfg = %+v, want dir=%q retain=%d interval=%v",
					cfg, tc.dir, tc.wantRetain, tc.wantInterval)
			}
		})
	}
}

// writeTempFile creates a plain file inside dir and returns its path (the
// "dir is a file" case above).
func writeTempFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "plain-file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestParseAlertWebhook pins the AN-10 fail-fast table (alert-notifier.md)
// and the AN-11 error hygiene: every rejection names the field and never
// contains the URL, the bearer, or the raw JSON.
func TestParseAlertWebhook(t *testing.T) {
	const secretURL = "https://hooks.example.com/T000/SECRET-URL-TOKEN"
	const secretBearer = "SECRET-BEARER-TOKEN"
	cases := []struct {
		name    string
		raw     string
		wantNil bool
		wantErr string
	}{
		{name: "unset disables", raw: "", wantNil: true},
		{name: "url only with defaults", raw: `{"url":"` + secretURL + `"}`},
		{name: "full config", raw: `{"url":"` + secretURL + `","authorization_bearer":"` + secretBearer +
			`","timeout_seconds":10,"poll_seconds":30,"max_per_tick":100,"heartbeat_hours":12,"log_only":false}`},
		{name: "log-only", raw: `{"log_only":true}`},
		{name: "log-only with knobs", raw: `{"log_only":true,"poll_seconds":60,"heartbeat_hours":0}`},
		{name: "not json", raw: `{"url":`, wantErr: "invalid JSON"},
		{name: "unknown field", raw: `{"url":"` + secretURL + `","bogus":1}`, wantErr: "invalid JSON"},
		{name: "url required", raw: `{}`, wantErr: "url is REQUIRED"},
		{name: "empty url", raw: `{"url":""}`, wantErr: "url is REQUIRED"},
		{name: "bad scheme", raw: `{"url":"ftp://` + secretBearer + `.example.com/hook"}`, wantErr: "scheme"},
		{name: "no host", raw: `{"url":"https:///hook"}`, wantErr: "host"},
		{name: "userinfo rejected", raw: `{"url":"https://user:` + secretBearer + `@example.com/hook"}`, wantErr: "userinfo"},
		{name: "log_only plus url", raw: `{"log_only":true,"url":"` + secretURL + `"}`, wantErr: "url MUST be absent"},
		{name: "log_only plus bearer", raw: `{"log_only":true,"authorization_bearer":"` + secretBearer + `"}`,
			wantErr: "authorization_bearer MUST be absent"},
		{name: "bearer http non-loopback", raw: `{"url":"http://internal.example.com/hook","authorization_bearer":"` +
			secretBearer + `"}`, wantErr: "loopback"},
		{name: "bearer http loopback ip", raw: `{"url":"http://127.0.0.1:9000/hook","authorization_bearer":"` + secretBearer + `"}`},
		{name: "bearer http localhost", raw: `{"url":"http://localhost:9000/hook","authorization_bearer":"` + secretBearer + `"}`},
		{name: "bearer http v6 loopback", raw: `{"url":"http://[::1]:9000/hook","authorization_bearer":"` + secretBearer + `"}`},
		{name: "bearer https any host", raw: `{"url":"` + secretURL + `","authorization_bearer":"` + secretBearer + `"}`},
		{name: "timeout low", raw: `{"url":"` + secretURL + `","timeout_seconds":0}`, wantErr: "timeout_seconds"},
		{name: "timeout high", raw: `{"url":"` + secretURL + `","timeout_seconds":61}`, wantErr: "timeout_seconds"},
		{name: "poll low", raw: `{"url":"` + secretURL + `","poll_seconds":0}`, wantErr: "poll_seconds"},
		{name: "poll high", raw: `{"url":"` + secretURL + `","poll_seconds":301}`, wantErr: "poll_seconds"},
		{name: "max_per_tick low", raw: `{"url":"` + secretURL + `","max_per_tick":0}`, wantErr: "max_per_tick"},
		{name: "max_per_tick high", raw: `{"url":"` + secretURL + `","max_per_tick":501}`, wantErr: "max_per_tick"},
		{name: "heartbeat negative", raw: `{"url":"` + secretURL + `","heartbeat_hours":-1}`, wantErr: "heartbeat_hours"},
		{name: "heartbeat high", raw: `{"url":"` + secretURL + `","heartbeat_hours":169}`, wantErr: "heartbeat_hours"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseAlertWebhook(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				for _, secret := range []string{secretURL, secretBearer, tc.raw} {
					if secret != "" && strings.Contains(err.Error(), secret) {
						t.Fatalf("error text leaks config material: %q", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAlertWebhook: %v", err)
			}
			if tc.wantNil {
				if cfg != nil {
					t.Fatalf("cfg = %+v, want nil (disabled)", cfg)
				}
				return
			}
			if cfg == nil {
				t.Fatal("cfg = nil, want parsed config")
			}
			if cfg.logOnly {
				if cfg.url != "" || cfg.bearer != "" {
					t.Fatalf("log-only cfg carries url/bearer: %+v", cfg)
				}
			} else if cfg.url == "" {
				t.Fatalf("webhook cfg missing url: %+v", cfg)
			}
		})
	}
	// Defaults (AN-10 table) and explicit values land as durations.
	cfg, err := parseAlertWebhook(`{"url":"https://ops.example.com/hook"}`)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.timeout != 5*time.Second || cfg.poll != 5*time.Second ||
		cfg.maxPerTick != 20 || cfg.heartbeat != 24*time.Hour || cfg.logOnly {
		t.Errorf("defaults = %+v, want 5s/5s/20/24h/webhook", cfg)
	}
	cfg, err = parseAlertWebhook(`{"url":"https://ops.example.com/hook","timeout_seconds":60,"poll_seconds":300,"max_per_tick":500,"heartbeat_hours":168}`)
	if err != nil {
		t.Fatalf("upper bounds: %v", err)
	}
	if cfg.timeout != 60*time.Second || cfg.poll != 300*time.Second ||
		cfg.maxPerTick != 500 || cfg.heartbeat != 168*time.Hour {
		t.Errorf("upper bounds = %+v, want 60s/300s/500/168h", cfg)
	}
	cfg, err = parseAlertWebhook(`{"url":"https://ops.example.com/hook","heartbeat_hours":0}`)
	if err != nil || cfg.heartbeat != 0 {
		t.Errorf("heartbeat_hours 0 = %+v err=%v, want disabled heartbeat", cfg, err)
	}
}

// TestParseMaxStrategiesPerTenant pins the SP-4b fail-closed cap parse:
// unset yields the default 100; 0, negatives and junk refuse to start
// (strategy-provisioning.md SP-4b).
func TestParseMaxStrategiesPerTenant(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 100, false}, {"1", 1, false}, {"7", 7, false}, {"250", 250, false},
		{"0", 0, true}, {"-3", 0, true}, {"junk", 0, true}, {"1.5", 0, true},
	} {
		got, err := parseMaxStrategiesPerTenant(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parse(%q): err = nil, want refusal", tc.raw)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parse(%q) = %d, %v, want %d, nil", tc.raw, got, err, tc.want)
		}
	}
}
