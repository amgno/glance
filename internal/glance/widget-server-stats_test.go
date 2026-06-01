package glance

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchBeszelServerInfo(t *testing.T) {
	authRequests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/collections/users/auth-with-password":
			authRequests++
			var credentials map[string]string
			if err := json.NewDecoder(r.Body).Decode(&credentials); err != nil {
				t.Fatalf("decode credentials: %v", err)
			}
			if credentials["identity"] != "test@example.com" || credentials["password"] != "secret" {
				t.Fatalf("unexpected credentials: %#v", credentials)
			}
			writeJSON(t, w, `{"token":"cached-token"}`)
		case "/api/collections/systems/records":
			requireAuthorization(t, r)
			writeJSON(t, w, `{"items":[{"id":"system-1","name":"homelab","status":"up","info":{"u":3600}}]}`)
		case "/api/collections/system_stats/records":
			requireAuthorization(t, r)
			if r.URL.Query().Get("filter") != `system="system-1" && type="1m"` {
				t.Fatalf("unexpected stats filter: %s", r.URL.Query().Get("filter"))
			}
			writeJSON(t, w, `{"items":[{"stats":{"cpu":42,"la":[2,0,4],"m":8,"mu":4,"mp":50,"s":2,"su":0.5,"d":100,"du":70,"dp":70,"t":{"cpu":81},"efs":{"/mnt/data":{"d":200,"du":20}}}}]}`)
		case "/api/collections/system_details/records/system-1":
			requireAuthorization(t, r)
			writeJSON(t, w, `{"hostname":"server.example","os_name":"Linux","threads":4}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	request := &serverStatsRequest{
		Provider: "beszel",
		URL:      server.URL,
		SystemID: "homelab",
		Username: "test@example.com",
		Password: "secret",
		Timeout:  durationField(time.Second),
	}

	info, err := fetchRemoteServerInfo(request)
	if err != nil {
		t.Fatalf("fetch Beszel server info: %v", err)
	}
	if info.Hostname != "server.example" || info.Platform != "Linux" {
		t.Fatalf("unexpected host info: %#v", info)
	}
	if info.CPU.Load1Percent != 50 || info.CPU.Load15Percent != 100 || info.CPU.TemperatureC != 81 {
		t.Fatalf("unexpected CPU info: %#v", info.CPU)
	}
	if info.Memory.TotalMB != 8192 || info.Memory.UsedMB != 4096 || info.Memory.SwapUsedPercent != 25 {
		t.Fatalf("unexpected memory info: %#v", info.Memory)
	}
	if len(info.Mountpoints) != 2 || info.Mountpoints[0].Path != "/" || info.Mountpoints[1].Path != "/mnt/data" {
		t.Fatalf("unexpected mountpoints: %#v", info.Mountpoints)
	}

	if _, err := fetchRemoteServerInfo(request); err != nil {
		t.Fatalf("fetch Beszel server info with cached token: %v", err)
	}
	if authRequests != 1 {
		t.Fatalf("expected one authentication request, got %d", authRequests)
	}
}

func TestConvertBeszelServerInfoUsesLegacyMetadata(t *testing.T) {
	system := &beszelSystem{
		Name: "fallback",
		Info: beszelSystemInfo{
			Hostname:    "legacy.example",
			Platform:    "debian",
			Threads:     2,
			LoadAverage: [3]float64{1, 0, 1.5},
			Uptime:      60,
			Temperature: 70,
		},
	}

	info := convertBeszelServerInfo(system, &beszelSystemDetails{}, &beszelSystemStats{
		MemoryTotalGB: 4,
		MemoryUsedGB:  1,
		MemoryPercent: 25,
		DiskTotalGB:   20,
		DiskUsedGB:    10,
		DiskPercent:   50,
	})

	if info.Hostname != "legacy.example" || info.Platform != "debian" {
		t.Fatalf("unexpected legacy host info: %#v", info)
	}
	if info.CPU.Load1Percent != 50 || info.CPU.Load15Percent != 75 {
		t.Fatalf("unexpected legacy CPU info: %#v", info.CPU)
	}
}

func TestFetchBeszelServerInfoDoesNotRefreshTokenForOfflineSystem(t *testing.T) {
	authRequests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/collections/users/auth-with-password":
			authRequests++
			writeJSON(t, w, `{"token":"cached-token"}`)
		case "/api/collections/systems/records":
			requireAuthorization(t, r)
			writeJSON(t, w, `{"items":[{"id":"system-1","name":"homelab","status":"down"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := fetchBeszelServerInfo(&serverStatsRequest{
		URL:      server.URL,
		SystemID: "homelab",
		Username: "test@example.com",
		Password: "secret",
		Timeout:  durationField(time.Second),
	})
	if err == nil {
		t.Fatal("expected an error for an offline Beszel system")
	}
	if authRequests != 1 {
		t.Fatalf("expected one authentication request, got %d", authRequests)
	}
}

func requireAuthorization(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("Authorization") != "Bearer cached-token" {
		t.Fatalf("unexpected authorization header: %q", r.Header.Get("Authorization"))
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write JSON response: %v", err)
	}
}
