// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/common/expfmt"

	pprof_runtime "runtime/pprof"
	template_text "text/template"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/route"
	"golang.org/x/net/context"
	"golang.org/x/net/netutil"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/retrieval"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/storage/local"
	"github.com/prometheus/prometheus/template"
	"github.com/prometheus/prometheus/util/httputil"
	"github.com/prometheus/prometheus/util/stats"
	api_v1 "github.com/prometheus/prometheus/web/api/v1"
	"github.com/prometheus/prometheus/web/ui"
)

const ModuleName = "web"

var localhostRepresentations = []string{"127.0.0.1", "localhost"}

// Handler serves various HTTP endpoints of the Prometheus server
type Handler struct {
	targetManager *retrieval.TargetManager
	ruleManager   *rules.Manager
	queryEngine   *promql.Engine
	context       context.Context
	storage       local.Storage
	notifier      *notifier.Notifier

	apiV1 *api_v1.API

	router       *route.Router
	listenErrCh  chan error
	quitCh       chan struct{}
	reloadCh     chan chan error
	options      *Options
	configString string
	versionInfo  *PrometheusVersion
	birth        time.Time
	cwd          string
	flagsMap     map[string]string

	externalLabels model.LabelSet
	mtx            sync.RWMutex
	now            func() model.Time
	reloadCount    int64
}

// ApplyConfig updates the status state as the new config requires.
func (h *Handler) ApplyConfig(conf *config.Config) error {
	if atomic.AddInt64(&h.reloadCount, 1) > 1 &&
		!conf.GlobalConfig.NeedsReloading(ModuleName) {
		return nil
	}
	h.mtx.Lock()
	defer h.mtx.Unlock()

	h.externalLabels = conf.GlobalConfig.ExternalLabels
	h.configString = conf.String()

	return nil
}

// PrometheusVersion contains build information about Prometheus.
type PrometheusVersion struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Branch    string `json:"branch"`
	BuildUser string `json:"buildUser"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

// Options for the web Handler.
type Options struct {
	Context       context.Context
	Storage       local.Storage
	QueryEngine   *promql.Engine
	TargetManager *retrieval.TargetManager
	RuleManager   *rules.Manager
	Notifier      *notifier.Notifier
	Version       *PrometheusVersion
	Flags         map[string]string

	ListenAddress        string
	ReadTimeout          time.Duration
	MaxConnections       int
	ExternalURL          *url.URL
	RoutePrefix          string
	MetricsPath          string
	UseLocalAssets       bool
	UserAssetsPath       string
	ConsoleTemplatesPath string
	ConsoleLibrariesPath string
	EnableQuit           bool
}

// New initializes a new web Handler.
func New(o *Options) *Handler {
	router := route.New(func(r *http.Request) (context.Context, error) {
		return o.Context, nil
	})

	cwd, err := os.Getwd()

	if err != nil {
		cwd = "<error retrieving current working directory>"
	}

	h := &Handler{
		router:      router,
		listenErrCh: make(chan error),
		quitCh:      make(chan struct{}),
		reloadCh:    make(chan chan error),
		options:     o,
		versionInfo: o.Version,
		birth:       time.Now(),
		cwd:         cwd,
		flagsMap:    o.Flags,

		context:       o.Context,
		targetManager: o.TargetManager,
		ruleManager:   o.RuleManager,
		queryEngine:   o.QueryEngine,
		storage:       o.Storage,
		notifier:      o.Notifier,

		apiV1: api_v1.NewAPI(o.QueryEngine, o.Storage, o.TargetManager, o.Notifier),
		now:   model.Now,
	}

	if o.RoutePrefix != "/" {
		// If the prefix is missing for the root path, prepend it.
		router.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, o.RoutePrefix, http.StatusFound)
		})
		router = router.WithPrefix(o.RoutePrefix)
	}

	instrh := prometheus.InstrumentHandler
	instrf := prometheus.InstrumentHandlerFunc

	router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		router.Redirect(w, r, "/graph", http.StatusFound)
	})

	router.Post("/v1/remote_push", instrf("remote_push", h.remotePush))
	router.Get("/v1/alerts.json", instrf("alerts", h.jsonAlerts))
	router.Get("/alerts", instrf("alerts", h.alerts))
	router.Get("/graph", instrf("graph", h.graph))
	router.Get("/status", instrf("status", h.status))
	router.Get("/flags", instrf("flags", h.flags))
	router.Get("/config", instrf("config", h.config))
	router.Get("/rules", instrf("rules", h.rules))
	router.Get("/targets", instrf("targets", h.targets))
	router.Get("/version", instrf("version", h.version))

	router.Get("/heap", instrf("heap", dumpHeap))

	router.Get(o.MetricsPath, prometheus.Handler().ServeHTTP)

	router.Get("/federate", instrh("federate", httputil.CompressionHandler{
		Handler: http.HandlerFunc(h.federation),
	}))

	h.apiV1.Register(router.WithPrefix("/api/v1"))

	router.Get("/consoles/*filepath", instrf("consoles", h.consoles))

	router.Get("/static/*filepath", instrf("static", serveStaticAsset))

	if o.UserAssetsPath != "" {
		router.Get("/user/*filepath", instrf("user", route.FileServe(o.UserAssetsPath)))
	}

	if o.EnableQuit {
		router.Post("/-/quit", h.quit)
	}

	router.Post("/-/reload", h.reload)
	router.Get("/-/reload", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintf(w, "This endpoint requires a POST request.\n")
	})

	router.Get("/debug/*subpath", http.DefaultServeMux.ServeHTTP)
	router.Post("/debug/*subpath", http.DefaultServeMux.ServeHTTP)

	return h
}

func serveStaticAsset(w http.ResponseWriter, req *http.Request) {
	fp := route.Param(route.Context(req), "filepath")
	fp = filepath.Join("web/ui/static", fp)

	info, err := ui.AssetInfo(fp)
	if err != nil {
		log.With("file", fp).Warn("Could not get file info: ", err)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	file, err := ui.Asset(fp)
	if err != nil {
		if err != io.EOF {
			log.With("file", fp).Warn("Could not get file: ", err)
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}

	http.ServeContent(w, req, info.Name(), info.ModTime(), bytes.NewReader(file))
}

// ListenError returns the receive-only channel that signals errors while starting the web server.
func (h *Handler) ListenError() <-chan error {
	return h.listenErrCh
}

// Quit returns the receive-only quit channel.
func (h *Handler) Quit() <-chan struct{} {
	return h.quitCh
}

// Reload returns the receive-only channel that signals configuration reload requests.
func (h *Handler) Reload() <-chan chan error {
	return h.reloadCh
}

// Run serves the HTTP endpoints.
func (h *Handler) Run() {
	log.Infof("Listening on %s", h.options.ListenAddress)
	server := &http.Server{
		Addr:        h.options.ListenAddress,
		Handler:     h.router,
		ErrorLog:    log.NewErrorLogger(),
		ReadTimeout: h.options.ReadTimeout,
	}
	listener, err := net.Listen("tcp", h.options.ListenAddress)
	if err != nil {
		h.listenErrCh <- err
	} else {
		limitedListener := netutil.LimitListener(listener, h.options.MaxConnections)
		h.listenErrCh <- server.Serve(limitedListener)
	}
}

func (h *Handler) alerts(w http.ResponseWriter, r *http.Request) {
	alerts := h.ruleManager.AlertingRules()
	alertsSorter := byAlertStateAndNameSorter{alerts: alerts}
	sort.Sort(alertsSorter)

	alertStatus := AlertStatus{
		AlertingRules: alertsSorter.alerts,
		AlertStateToRowClass: map[rules.AlertState]string{
			rules.StateInactive: "success",
			rules.StatePending:  "warning",
			rules.StateFiring:   "danger",
		},
	}
	h.executeTemplate(w, "alerts.html", alertStatus)
}

func respErr(w http.ResponseWriter, err error) {
	h := w.Header()
	h.Set("Content-Type", "application/json")

	if err == nil {
		w.WriteHeader(200)
		w.Write([]byte{'{', '}'})
		return
	}
	w.WriteHeader(500)
	ret := map[string]string{}
	ret["error"] = err.Error()
	b, _ := json.Marshal(ret)
	w.Write(b)
	return
}

func (h *Handler) remotePush(w http.ResponseWriter, r *http.Request) {

	var err error
	if h.storage.NeedsThrottling() {
		err = errors.New("storage needs throttling")
		respErr(w, err)
		return
	}

	decSamples := make(model.Vector, 0, 50)
	allSamples := make(model.Samples, 0, 200)

	proto := expfmt.ResponseFormat(r.Header)
	sdec := expfmt.SampleDecoder{
		Dec: expfmt.NewDecoder(r.Body, proto),
		Opts: &expfmt.DecodeOptions{
			Timestamp: model.Now(),
		},
	}

	for {
		if err = sdec.Decode(&decSamples); err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
		allSamples = append(allSamples, decSamples...)
		decSamples = decSamples[:0]
	}

	var (
		numTotal      = 0
		numOutOfOrder = 0
		numDuplicates = 0
	)

	for _, s := range allSamples {
		numTotal++
		if err := h.storage.Append(s); err != nil {
			switch err {
			case local.ErrOutOfOrderSample:
				numOutOfOrder++
				log.With("sample", s).With("error", err).Debug("Remote Sample discarded")
			case local.ErrDuplicateSampleForTimestamp:
				numDuplicates++
				log.With("sample", s).With("error", err).Debug("Remote Sample discarded")
			default:
				log.With("sample", s).With("error", err).Warn("Remote Sample discarded")
			}
		}
	}
	if numOutOfOrder > 0 {
		log.With("numDropped", numOutOfOrder).Warn("Error on ingesting out-of-order remote samples")
	}
	if numDuplicates > 0 {
		log.With("numDropped", numDuplicates).Warn("Error on ingesting remote samples with different value but same timestamp")
	}
	err = nil
	respErr(w, err)
	return
}

func (h *Handler) jsonAlerts(w http.ResponseWriter, r *http.Request) {
	type alert struct {
		Name   string            `json:"name"`
		Metric map[string]string `json:"metric"`
		Value  float64           `json:"value"`
		Status int               `json:"status"`
	}
	type alertsPerRule struct {
		Rule   *rules.ALertingRuleDesc `json:"rule"`
		Alerts []*alert                `json:"alerts"`
		Stats  *stats.QueryStats       `json:"stats"`
	}
	ret := map[string]alertsPerRule{}
	rules := h.ruleManager.AlertingRules()
	for _, x := range rules {
		alts, ok := ret[x.Name()]
		if !ok {
			alts = alertsPerRule{
				Rule:  x.Desc(),
				Stats: x.LastStats(),
			}
			ret[x.Name()] = alts
		}
		for _, y := range x.ActiveAlerts() {
			if math.IsNaN(float64(y.Value)) || math.IsInf(float64(y.Value), 0) {
				y.Value = 42.0
			}
			r := &alert{
				Name:   x.Name(),
				Metric: map[string]string{},
				Value:  float64(y.Value),
				Status: int(y.State),
			}
			for k, v := range y.Labels {
				r.Metric[string(k)] = string(v)
			}
			alts.Alerts = append(alts.Alerts, r)
		}
		ret[x.Name()] = alts
	}
	b, err := json.Marshal(ret)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

func (h *Handler) consoles(w http.ResponseWriter, r *http.Request) {
	ctx := route.Context(r)
	name := route.Param(ctx, "filepath")

	file, err := http.Dir(h.options.ConsoleTemplatesPath).Open(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	text, err := ioutil.ReadAll(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Provide URL parameters as a map for easy use. Advanced users may have need for
	// parameters beyond the first, so provide RawParams.
	rawParams, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	params := map[string]string{}
	for k, v := range rawParams {
		params[k] = v[0]
	}
	data := struct {
		RawParams url.Values
		Params    map[string]string
		Path      string
	}{
		RawParams: rawParams,
		Params:    params,
		Path:      strings.TrimLeft(name, "/"),
	}

	tmpl := template.NewTemplateExpander(h.context, string(text), "__console_"+name, data, h.now(), h.queryEngine, h.options.ExternalURL.Path)
	filenames, err := filepath.Glob(h.options.ConsoleLibrariesPath + "/*.lib")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result, err := tmpl.ExpandHTML(filenames)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, result)
}

func (h *Handler) graph(w http.ResponseWriter, r *http.Request) {
	h.executeTemplate(w, "graph.html", nil)
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	h.executeTemplate(w, "status.html", struct {
		Birth         time.Time
		CWD           string
		Version       *PrometheusVersion
		Alertmanagers []string
	}{
		Birth:         h.birth,
		CWD:           h.cwd,
		Version:       h.versionInfo,
		Alertmanagers: h.notifier.Alertmanagers(),
	})
}

func (h *Handler) flags(w http.ResponseWriter, r *http.Request) {
	h.executeTemplate(w, "flags.html", h.flagsMap)
}

func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	h.mtx.RLock()
	defer h.mtx.RUnlock()

	h.executeTemplate(w, "config.html", h.configString)
}

func (h *Handler) rules(w http.ResponseWriter, r *http.Request) {
	h.executeTemplate(w, "rules.html", h.ruleManager)
}

func (h *Handler) targets(w http.ResponseWriter, r *http.Request) {
	// Bucket targets by job label
	tps := map[string][]*retrieval.Target{}
	for _, t := range h.targetManager.Targets() {
		job := string(t.Labels()[model.JobLabel])
		tps[job] = append(tps[job], t)
	}

	h.executeTemplate(w, "targets.html", struct {
		TargetPools map[string][]*retrieval.Target
	}{
		TargetPools: tps,
	})
}

func (h *Handler) version(w http.ResponseWriter, r *http.Request) {
	dec := json.NewEncoder(w)
	if err := dec.Encode(h.versionInfo); err != nil {
		http.Error(w, fmt.Sprintf("error encoding JSON: %s", err), http.StatusInternalServerError)
	}
}

func (h *Handler) quit(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Requesting termination... Goodbye!")
	close(h.quitCh)
}

func (h *Handler) reload(w http.ResponseWriter, r *http.Request) {
	rc := make(chan error)
	h.reloadCh <- rc
	if err := <-rc; err != nil {
		http.Error(w, fmt.Sprintf("failed to reload config: %s", err), http.StatusInternalServerError)
	}
}

func (h *Handler) consolesPath() string {
	if _, err := os.Stat(h.options.ConsoleTemplatesPath + "/index.html"); !os.IsNotExist(err) {
		return h.options.ExternalURL.Path + "/consoles/index.html"
	}
	if h.options.UserAssetsPath != "" {
		if _, err := os.Stat(h.options.UserAssetsPath + "/index.html"); !os.IsNotExist(err) {
			return h.options.ExternalURL.Path + "/user/index.html"
		}
	}
	return ""
}

func tmplFuncs(consolesPath string, opts *Options) template_text.FuncMap {
	return template_text.FuncMap{
		"since": func(t time.Time) time.Duration {
			return time.Since(t) / time.Millisecond * time.Millisecond
		},
		"consolesPath": func() string { return consolesPath },
		"pathPrefix":   func() string { return opts.ExternalURL.Path },
		"stripLabels": func(lset model.LabelSet, labels ...model.LabelName) model.LabelSet {
			for _, ln := range labels {
				delete(lset, ln)
			}
			return lset
		},
		"globalURL": func(u *url.URL) *url.URL {
			host, port, err := net.SplitHostPort(u.Host)
			if err != nil {
				return u
			}
			for _, lhr := range localhostRepresentations {
				if host == lhr {
					_, ownPort, err := net.SplitHostPort(opts.ListenAddress)
					if err != nil {
						return u
					}

					if port == ownPort {
						// Only in the case where the target is on localhost and its port is
						// the same as the one we're listening on, we know for sure that
						// we're monitoring our own process and that we need to change the
						// scheme, hostname, and port to the externally reachable ones as
						// well. We shouldn't need to touch the path at all, since if a
						// path prefix is defined, the path under which we scrape ourselves
						// should already contain the prefix.
						u.Scheme = opts.ExternalURL.Scheme
						u.Host = opts.ExternalURL.Host
					} else {
						// Otherwise, we only know that localhost is not reachable
						// externally, so we replace only the hostname by the one in the
						// external URL. It could be the wrong hostname for the service on
						// this port, but it's still the best possible guess.
						host, _, err := net.SplitHostPort(opts.ExternalURL.Host)
						if err != nil {
							return u
						}
						u.Host = host + ":" + port
					}
					break
				}
			}
			return u
		},
		"healthToClass": func(th retrieval.TargetHealth) string {
			switch th {
			case retrieval.HealthUnknown:
				return "warning"
			case retrieval.HealthGood:
				return "success"
			default:
				return "danger"
			}
		},
		"alertStateToClass": func(as rules.AlertState) string {
			switch as {
			case rules.StateInactive:
				return "success"
			case rules.StatePending:
				return "warning"
			case rules.StateFiring:
				return "danger"
			default:
				panic("unknown alert state")
			}
		},
	}
}

func (h *Handler) getTemplate(name string) (string, error) {
	baseTmpl, err := ui.Asset("web/ui/templates/_base.html")
	if err != nil {
		return "", fmt.Errorf("error reading base template: %s", err)
	}
	pageTmpl, err := ui.Asset(filepath.Join("web/ui/templates", name))
	if err != nil {
		return "", fmt.Errorf("error reading page template %s: %s", name, err)
	}
	return string(baseTmpl) + string(pageTmpl), nil
}

func (h *Handler) executeTemplate(w http.ResponseWriter, name string, data interface{}) {
	text, err := h.getTemplate(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	tmpl := template.NewTemplateExpander(h.context, text, name, data, h.now(), h.queryEngine, h.options.ExternalURL.Path)
	tmpl.Funcs(tmplFuncs(h.consolesPath(), h.options))

	result, err := tmpl.ExpandHTML(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, result)
}

func dumpHeap(w http.ResponseWriter, r *http.Request) {
	target := fmt.Sprintf("/tmp/%d.heap", time.Now().Unix())
	f, err := os.Create(target)
	if err != nil {
		log.Error("Could not dump heap: ", err)
	}
	fmt.Fprintf(w, "Writing to %s...", target)
	defer f.Close()
	pprof_runtime.WriteHeapProfile(f)
	fmt.Fprintf(w, "Done")
}

// AlertStatus bundles alerting rules and the mapping of alert states to row classes.
type AlertStatus struct {
	AlertingRules        []*rules.AlertingRule
	AlertStateToRowClass map[rules.AlertState]string
}

type byAlertStateAndNameSorter struct {
	alerts []*rules.AlertingRule
}

func (s byAlertStateAndNameSorter) Len() int {
	return len(s.alerts)
}

func (s byAlertStateAndNameSorter) Less(i, j int) bool {
	return s.alerts[i].State() > s.alerts[j].State() ||
		(s.alerts[i].State() == s.alerts[j].State() &&
			s.alerts[i].Name() < s.alerts[j].Name())
}

func (s byAlertStateAndNameSorter) Swap(i, j int) {
	s.alerts[i], s.alerts[j] = s.alerts[j], s.alerts[i]
}
