package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var pdStates = map[string]string{
	"UGood":  "unconf_good",
	"UBad":   "unconf_bad",
	"HSP":    "hotspare",
	"Offln":  "offline",
	"Onln":   "online",
	"Rbld":   "rebuild",
	"Failed": "failed",
	"GHS":    "global_hotspare",
	"DHS":    "dedicated_hotspare",
}

var vdStates = map[string]string{
	"Optl": "optimal",
	"OfLn": "offline",
	"Pdgd": "partial_degraded",
	"Dgrd": "degraded",
}

// ---------------------------------------------------------------------------
// Type for CLI output
// ---------------------------------------------------------------------------

type storcliOutput struct {
	Controllers []controllerData `json:"Controllers"`
}

type controllerData struct {
	CommandStatus *struct {
		Controller int `json:"Controller"`
	} `json:"Command Status"`
	ResponseData *struct {
		ResponseData *struct {
			VDList []vdEntry `json:"VD LIST"`
			DGList []dgEntry `json:"DG Drive LIST"`
		} `json:"Response Data"`
	} `json:"Response Data"`
}

type vdEntry struct {
	State string `json:"State"`
}

type dgEntry struct {
	State string      `json:"State"`
	DG    interface{} `json:"DG"`
}

// ---------------------------------------------------------------------------
// Collector for states
// ---------------------------------------------------------------------------

type Collector interface {
	AddVirtualDrive(cID int, vd vdEntry)
	AddDGDrive(cID int, dg dgEntry)
}

type StateMetrics struct {
	States   map[string]int
	Count    int
	Instance string
}

func NewStateMetrics(instance string) *StateMetrics {
	return &StateMetrics{States: map[string]int{}, Instance: instance}
}

func (s *StateMetrics) AddState(state string) {
	s.States[state]++
	s.Count++
}

// Simple Collector for aggregated Metrics
type MetricCollector struct {
	virtuals map[string]*StateMetrics
	drives   map[string]*StateMetrics
}

func NewMetricCollector() *MetricCollector {
	return &MetricCollector{
		virtuals: map[string]*StateMetrics{},
		drives:   map[string]*StateMetrics{},
	}
}

func (m *MetricCollector) AddVirtualDrive(cID int, vd vdEntry) {
	id := fmt.Sprint(cID)
	sm, ok := m.virtuals[id]
	if !ok {
		sm = NewStateMetrics(id)
		m.virtuals[id] = sm
	}
	if state, ok := vdStates[vd.State]; ok {
		sm.AddState(state)
	}
}

func (m *MetricCollector) AddDGDrive(cID int, dg dgEntry) {
	id := fmt.Sprintf("c%d_d%v", cID, dg.DG)
	sm, ok := m.drives[id]
	if !ok {
		sm = NewStateMetrics(id)
		m.drives[id] = sm
	}
	if state, ok := pdStates[dg.State]; ok {
		sm.AddState(state)
	}
}

// ---------------------------------------------------------------------------
// Fetcher
// ---------------------------------------------------------------------------

// Fetcher run the underlying cli tool and pass json result to collector
type Fetcher struct {
	StorcliBin string
	Timeout    time.Duration
}

func (f *Fetcher) fetch(args ...string) (*storcliOutput, error) {
	ctx, cancel := context.WithTimeout(context.Background(), f.Timeout)
	defer cancel()

	// storcli peut renvoyer un code != 0 avec un JSON valide :
	// on ne considère l'erreur que si la sortie est vide.
	out, err := exec.CommandContext(ctx, f.StorcliBin, args...).Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("exécution de storcli: %w", err)
	}
	var data storcliOutput
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("JSON invalide: %w", err)
	}
	return &data, nil
}

func (f *Fetcher) ReadDisks(c Collector, controller, disk string) error {
	opts := fmt.Sprintf("/c%s/d%s", controller, disk)
	data, err := f.fetch(opts, "show", "all", "nolog", "J")
	if err != nil {
		return err
	}
	if data.Controllers == nil {
		return fmt.Errorf("entrée 'Controllers' introuvable")
	}
	for i, cd := range data.Controllers {
		if cd.CommandStatus == nil {
			return fmt.Errorf("Controllers/%d/Command Status introuvable", i)
		}
		cID := cd.CommandStatus.Controller
		if cd.ResponseData == nil || cd.ResponseData.ResponseData == nil {
			return fmt.Errorf("Controllers/%d/Response Data/Response Data introuvable", i)
		}
		r := cd.ResponseData.ResponseData
		for _, vd := range r.VDList {
			c.AddVirtualDrive(cID, vd)
		}
		for _, dg := range r.DGList {
			c.AddDGDrive(cID, dg)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Exporter Prometheus
// ---------------------------------------------------------------------------

type Exporter struct {
	fetcher    *Fetcher
	controller string
	disk       string
	mu         sync.Mutex // Avoir concurrent execution of cli tool

	vdState        *prometheus.Desc
	pdState        *prometheus.Desc
	scrapeSuccess  *prometheus.Desc
	scrapeDuration *prometheus.Desc
	allVDStates    []string
	allPDStates    []string
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func NewExporter(f *Fetcher, controller, disk string) *Exporter {
	return &Exporter{
		fetcher:    f,
		controller: controller,
		disk:       disk,
		vdState: prometheus.NewDesc("megaraid_virtual_drive_state",
			"Count of virtual drive by state", []string{"state", "virtual_drive"}, nil),
		pdState: prometheus.NewDesc("megaraid_physical_drive_state",
			"Count of physical drive by state", []string{"state", "physical_drive"}, nil),
		scrapeSuccess: prometheus.NewDesc("megaraid_scrape_success",
			"Success of the scraping (1=success)", nil, nil),
		scrapeDuration: prometheus.NewDesc("megaraid_scrape_duration_seconds",
			"Duration of the tool exceution", nil, nil),
		allVDStates: sortedValues(vdStates),
		allPDStates: sortedValues(pdStates),
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.vdState
	ch <- e.pdState
	ch <- e.scrapeSuccess
	ch <- e.scrapeDuration
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mu.Lock()
	defer e.mu.Unlock()

	start := time.Now()
	mc := NewMetricCollector()
	err := e.fetcher.ReadDisks(mc, e.controller, e.disk)
	duration := time.Since(start).Seconds()

	success := 1.0
	if err != nil {
		success = 0
		log.Printf("erreur de collecte: %v", err)
	} else {
		emit := func(desc *prometheus.Desc, all []string, m map[string]*StateMetrics) {
			for _, sm := range m {
				for _, state := range all {
					ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue,
						float64(sm.States[state]), state, sm.Instance)
				}
			}
		}
		emit(e.vdState, e.allVDStates, mc.virtuals)
		emit(e.pdState, e.allPDStates, mc.drives)
	}

	ch <- prometheus.MustNewConstMetric(e.scrapeSuccess, prometheus.GaugeValue, success)
	ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, duration)
}

// ---------------------------------------------------------------------------

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	listen := flag.String("web.listen-address", "127.0.0.1:9333", "Listening address")
	path := flag.String("web.telemetry-path", "/metrics", "web path on which expose metrics")
	bin := flag.String("storcli", "/opt/MegaRAID/storcli/storcli64", "Path to storcli64 (or like) tool")
	controller := flag.String("controller", "all", "Controller selection")
	disk := flag.String("disk", "all", "Disk Selection")
	timeout := flag.Duration("timeout", 30*time.Second, "timeout to run storcli tool")
	versionCommand := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *versionCommand {
		fmt.Printf("megaraid-exporter %s, commit %s, built at %s", version, commit, date)
		return
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(NewExporter(
		&Fetcher{StorcliBin: *bin, Timeout: *timeout}, *controller, *disk))

	mux := http.NewServeMux()
	mux.Handle(*path, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body><a href="%s">Metrics</a>Megaraid Exporter %s (%s)</body></html>`, *path, version, commit)
	})

	log.Printf("megaraid-exporter %s, commit %s, built at %s", version, commit, date)
	log.Printf("Listening at http://%s%s", *listen, *path)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
