package glance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/glanceapp/glance/pkg/sysinfo"
)

var serverStatsWidgetTemplate = mustParseTemplate("server-stats.html", "widget-base.html")

type serverStatsWidget struct {
	widgetBase `yaml:",inline"`
	Servers    []serverStatsRequest `yaml:"servers"`
}

func (widget *serverStatsWidget) initialize() error {
	widget.withTitle("Server Stats").withCacheDuration(15 * time.Second)
	widget.widgetBase.WIP = true

	if len(widget.Servers) == 0 {
		widget.Servers = []serverStatsRequest{{Type: "local"}}
	}

	for i := range widget.Servers {
		widget.Servers[i].URL = strings.TrimRight(widget.Servers[i].URL, "/")

		if widget.Servers[i].Timeout == 0 {
			widget.Servers[i].Timeout = durationField(3 * time.Second)
		}
	}

	return nil
}

func (widget *serverStatsWidget) update(context.Context) {
	// Refactor later, most of it may change depending on feedback
	var wg sync.WaitGroup

	for i := range widget.Servers {
		serv := &widget.Servers[i]

		if serv.Type == "local" {
			info, errs := sysinfo.Collect(serv.SystemInfoRequest)

			if len(errs) > 0 {
				for i := range errs {
					slog.Warn("Getting system info: " + errs[i].Error())
				}
			}

			serv.IsReachable = true
			serv.Info = info
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				info, err := fetchRemoteServerInfo(serv)
				if err != nil {
					slog.Warn("Getting remote system info: " + err.Error())
					serv.IsReachable = false
					serv.Info = &sysinfo.SystemInfo{
						Hostname: "Unnamed server #" + strconv.Itoa(i+1),
					}
				} else {
					serv.IsReachable = true
					serv.Info = info
				}
			}()
		}
	}

	wg.Wait()
	widget.withError(nil).scheduleNextUpdate()
}

func (widget *serverStatsWidget) Render() template.HTML {
	return widget.renderTemplate(widget, serverStatsWidgetTemplate)
}

type serverStatsRequest struct {
	*sysinfo.SystemInfoRequest `yaml:",inline"`
	Info                       *sysinfo.SystemInfo `yaml:"-"`
	IsReachable                bool                `yaml:"-"`
	StatusText                 string              `yaml:"-"`
	Name                       string              `yaml:"name"`
	HideSwap                   bool                `yaml:"hide-swap"`
	Type                       string              `yaml:"type"`
	URL                        string              `yaml:"url"`
	Token                      string              `yaml:"token"`
	Timeout                    durationField       `yaml:"timeout"`
	Provider                   string              `yaml:"provider"`
	SystemID                   string              `yaml:"system-id"`
	Username                   string              `yaml:"username"`
	Password                   string              `yaml:"password"`
	beszelToken                string
	beszelTokenMu              sync.Mutex
}

func fetchRemoteServerInfo(infoReq *serverStatsRequest) (*sysinfo.SystemInfo, error) {
	switch infoReq.Provider {
	case "", "glance":
		return fetchRemoteGlanceServerInfo(infoReq)
	case "beszel":
		return fetchBeszelServerInfo(infoReq)
	default:
		return nil, fmt.Errorf("unknown server stats provider: %s", infoReq.Provider)
	}
}

func fetchRemoteGlanceServerInfo(infoReq *serverStatsRequest) (*sysinfo.SystemInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(infoReq.Timeout))
	defer cancel()

	request, _ := http.NewRequestWithContext(ctx, "GET", infoReq.URL+"/api/sysinfo/all", nil)
	if infoReq.Token != "" {
		request.Header.Set("Authorization", "Bearer "+infoReq.Token)
	}

	info, err := decodeJsonFromRequest[*sysinfo.SystemInfo](defaultHTTPClient, request)
	if err != nil {
		return nil, err
	}

	return info, nil
}

type beszelAuthResponse struct {
	Token string `json:"token"`
}

type beszelSystemListResponse struct {
	Items []beszelSystem `json:"items"`
}

type beszelSystem struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Status string           `json:"status"`
	Info   beszelSystemInfo `json:"info"`
}

type beszelSystemInfo struct {
	Hostname         string             `json:"h"`
	Platform         string             `json:"o"`
	CPUPercent       float64            `json:"cpu"`
	Threads          int                `json:"t"`
	Cores            int                `json:"c"`
	LoadAverage      [3]float64         `json:"la"`
	Uptime           uint64             `json:"u"`
	MemoryPercent    float64            `json:"mp"`
	DiskPercent      float64            `json:"dp"`
	Temperature      float64            `json:"dt"`
	ExtraFilesystems map[string]float64 `json:"efs"`
}

type beszelSystemDetails struct {
	Hostname string `json:"hostname"`
	Platform string `json:"os_name"`
	Threads  int    `json:"threads"`
	Cores    int    `json:"cores"`
}

type beszelSystemStatsListResponse struct {
	Items []struct {
		Stats beszelSystemStats `json:"stats"`
	} `json:"items"`
}

type beszelSystemStats struct {
	CPUPercent       float64                          `json:"cpu"`
	LoadAverage      [3]float64                       `json:"la"`
	MemoryTotalGB    float64                          `json:"m"`
	MemoryUsedGB     float64                          `json:"mu"`
	MemoryPercent    float64                          `json:"mp"`
	SwapTotalGB      float64                          `json:"s"`
	SwapUsedGB       float64                          `json:"su"`
	DiskTotalGB      float64                          `json:"d"`
	DiskUsedGB       float64                          `json:"du"`
	DiskPercent      float64                          `json:"dp"`
	Temperatures     map[string]float64               `json:"t"`
	ExtraFilesystems map[string]beszelFilesystemStats `json:"efs"`
}

type beszelFilesystemStats struct {
	DiskTotalGB float64 `json:"d"`
	DiskUsedGB  float64 `json:"du"`
}

var errBeszelUnauthorized = errors.New("Beszel authorization failed")

func fetchBeszelServerInfo(infoReq *serverStatsRequest) (*sysinfo.SystemInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(infoReq.Timeout))
	defer cancel()

	token, err := infoReq.getBeszelToken(ctx, false)
	if err != nil {
		return nil, err
	}

	info, err := fetchBeszelServerInfoWithToken(ctx, infoReq, token)
	if err == nil || infoReq.Token != "" || !errors.Is(err, errBeszelUnauthorized) {
		return info, err
	}

	token, authErr := infoReq.getBeszelToken(ctx, true)
	if authErr != nil {
		return nil, authErr
	}

	return fetchBeszelServerInfoWithToken(ctx, infoReq, token)
}

func fetchBeszelServerInfoWithToken(ctx context.Context, infoReq *serverStatsRequest, token string) (*sysinfo.SystemInfo, error) {
	if infoReq.SystemID == "" {
		return nil, fmt.Errorf("system-id is required for the Beszel provider")
	}

	systems, err := fetchBeszelJSON[beszelSystemListResponse](ctx, infoReq.URL+"/api/collections/systems/records?page=1&perPage=500", token)
	if err != nil {
		return nil, err
	}

	var system *beszelSystem
	for i := range systems.Items {
		if systems.Items[i].ID == infoReq.SystemID || systems.Items[i].Name == infoReq.SystemID {
			system = &systems.Items[i]
			break
		}
	}
	if system == nil {
		return nil, fmt.Errorf("Beszel system not found: %s", infoReq.SystemID)
	}
	if system.Status != "up" {
		return nil, fmt.Errorf("Beszel system %s is %s", infoReq.SystemID, system.Status)
	}

	query := url.Values{}
	query.Set("page", "1")
	query.Set("perPage", "1")
	query.Set("sort", "-created")
	query.Set("filter", fmt.Sprintf(`system="%s" && type="1m"`, system.ID))
	statsList, err := fetchBeszelJSON[beszelSystemStatsListResponse](ctx, infoReq.URL+"/api/collections/system_stats/records?"+query.Encode(), token)
	if err != nil {
		return nil, err
	}
	if len(statsList.Items) == 0 {
		return nil, fmt.Errorf("Beszel returned no stats for system: %s", infoReq.SystemID)
	}

	details, _ := fetchBeszelJSON[beszelSystemDetails](ctx, infoReq.URL+"/api/collections/system_details/records/"+system.ID, token)

	return convertBeszelServerInfo(system, &details, &statsList.Items[0].Stats), nil
}

func (infoReq *serverStatsRequest) getBeszelToken(ctx context.Context, refresh bool) (string, error) {
	if infoReq.Token != "" {
		return infoReq.Token, nil
	}
	if infoReq.Username == "" || infoReq.Password == "" {
		return "", fmt.Errorf("username and password are required when a Beszel token is not configured")
	}

	infoReq.beszelTokenMu.Lock()
	defer infoReq.beszelTokenMu.Unlock()

	if infoReq.beszelToken != "" && !refresh {
		return infoReq.beszelToken, nil
	}

	body, _ := json.Marshal(map[string]string{
		"identity": infoReq.Username,
		"password": infoReq.Password,
	})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, infoReq.URL+"/api/collections/users/auth-with-password", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")

	auth, err := decodeJsonFromRequest[beszelAuthResponse](defaultHTTPClient, request)
	if err != nil {
		return "", err
	}
	if auth.Token == "" {
		return "", fmt.Errorf("Beszel authentication returned an empty token")
	}

	infoReq.beszelToken = auth.Token
	return auth.Token, nil
}

func fetchBeszelJSON[T any](ctx context.Context, requestURL, token string) (T, error) {
	var result T

	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := defaultHTTPClient.Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return result, err
	}

	if response.StatusCode != http.StatusOK {
		truncatedBody, _ := limitStringLength(string(body), 256)
		statusErr := fmt.Errorf(
			"unexpected status code %d from %s, response: %s",
			response.StatusCode,
			request.URL,
			truncatedBody,
		)
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return result, fmt.Errorf("%w: %v", errBeszelUnauthorized, statusErr)
		}
		return result, statusErr
	}

	err = json.Unmarshal(body, &result)
	return result, err
}

func convertBeszelServerInfo(system *beszelSystem, details *beszelSystemDetails, stats *beszelSystemStats) *sysinfo.SystemInfo {
	info := &sysinfo.SystemInfo{
		Hostname: system.Name,
	}

	info.Hostname = firstNonEmpty(details.Hostname, system.Info.Hostname, system.Name)
	info.Platform = firstNonEmpty(details.Platform, system.Info.Platform)
	info.HostInfoIsAvailable = system.Info.Uptime > 0
	if system.Info.Uptime > 0 {
		info.SetBootTime(time.Now().Add(-time.Duration(system.Info.Uptime) * time.Second))
	}

	cpuCount := firstPositive(details.Threads, details.Cores, system.Info.Threads, system.Info.Cores, 1)
	loadAverage := stats.LoadAverage
	if loadAverage == [3]float64{} {
		loadAverage = system.Info.LoadAverage
	}
	if loadAverage != [3]float64{} {
		info.CPU.LoadIsAvailable = true
		info.CPU.Load1Percent = percentage(loadAverage[0] / float64(cpuCount) * 100)
		info.CPU.Load15Percent = percentage(loadAverage[2] / float64(cpuCount) * 100)
	} else {
		cpuPercent := stats.CPUPercent
		if cpuPercent == 0 {
			cpuPercent = system.Info.CPUPercent
		}
		info.CPU.LoadIsAvailable = true
		info.CPU.Load1Percent = percentage(cpuPercent)
		info.CPU.Load15Percent = percentage(cpuPercent)
	}

	temperature := system.Info.Temperature
	if temperature == 0 {
		for _, value := range stats.Temperatures {
			temperature = max(temperature, value)
		}
	}
	if temperature != 0 {
		info.CPU.TemperatureIsAvailable = true
		info.CPU.TemperatureC = percentage(temperature)
	}

	memoryPercent := stats.MemoryPercent
	if memoryPercent == 0 {
		memoryPercent = system.Info.MemoryPercent
	}
	if stats.MemoryTotalGB > 0 || memoryPercent > 0 {
		info.Memory.IsAvailable = true
		info.Memory.TotalMB = gigabytesToMegabytes(stats.MemoryTotalGB)
		info.Memory.UsedMB = gigabytesToMegabytes(stats.MemoryUsedGB)
		info.Memory.UsedPercent = percentage(memoryPercent)
	}
	if stats.SwapTotalGB > 0 {
		info.Memory.SwapIsAvailable = true
		info.Memory.SwapTotalMB = gigabytesToMegabytes(stats.SwapTotalGB)
		info.Memory.SwapUsedMB = gigabytesToMegabytes(stats.SwapUsedGB)
		info.Memory.SwapUsedPercent = percentage(stats.SwapUsedGB / stats.SwapTotalGB * 100)
	}

	diskPercent := stats.DiskPercent
	if diskPercent == 0 {
		diskPercent = system.Info.DiskPercent
	}
	if stats.DiskTotalGB > 0 || diskPercent > 0 {
		info.Mountpoints = append(info.Mountpoints, sysinfo.MountpointInfo{
			Path:        "/",
			TotalMB:     gigabytesToMegabytes(stats.DiskTotalGB),
			UsedMB:      gigabytesToMegabytes(stats.DiskUsedGB),
			UsedPercent: percentage(diskPercent),
		})
	}
	for path, filesystem := range stats.ExtraFilesystems {
		if filesystem.DiskTotalGB <= 0 {
			continue
		}
		info.Mountpoints = append(info.Mountpoints, sysinfo.MountpointInfo{
			Path:        path,
			TotalMB:     gigabytesToMegabytes(filesystem.DiskTotalGB),
			UsedMB:      gigabytesToMegabytes(filesystem.DiskUsedGB),
			UsedPercent: percentage(filesystem.DiskUsedGB / filesystem.DiskTotalGB * 100),
		})
	}
	sort.Slice(info.Mountpoints, func(a, b int) bool {
		return info.Mountpoints[a].UsedPercent > info.Mountpoints[b].UsedPercent
	})

	return info
}

func percentage(value float64) uint8 {
	return uint8(math.Min(math.Max(math.Round(value), 0), 100))
}

func gigabytesToMegabytes(value float64) uint64 {
	return uint64(math.Round(value * 1024))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
