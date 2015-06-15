// Copyright 2015 The Prometheus Authors
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

// The main package for the Prometheus server executeable.
package main

import (
	"flag"
	_ "net/http/pprof" // Comment this line to disable pprof endpoint.
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/log"

	registry "github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/notification"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/retrieval"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/local"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/storage/remote/influxdb"
	"github.com/prometheus/prometheus/storage/remote/opentsdb"
	"github.com/prometheus/prometheus/web"
)

func main() {
	os.Exit(Main())
}

func Main() int {
	if err := parse(os.Args[1:]); err != nil {
		return 2
	}

	versionInfoTmpl.Execute(os.Stdout, BuildInfo)
	if cfg.printVersion {
		return 0
	}

	memStorage := local.NewMemorySeriesStorage(&cfg.storage)

	var (
		sampleAppender      storage.SampleAppender
		remoteStorageQueues []*remote.StorageQueueManager
	)
	if cfg.opentsdbURL == "" && cfg.influxdbURL == "" {
		log.Warnf("No remote storage URLs provided; not sending any samples to long-term storage")
		sampleAppender = memStorage
	} else {
		fanout := storage.Fanout{memStorage}

		addRemoteStorage := func(c remote.StorageClient) {
			qm := remote.NewStorageQueueManager(c, 100*1024)
			fanout = append(fanout, qm)
			remoteStorageQueues = append(remoteStorageQueues, qm)
		}

		if cfg.opentsdbURL != "" {
			addRemoteStorage(opentsdb.NewClient(cfg.opentsdbURL, cfg.remoteStorageTimeout))
		}
		if cfg.influxdbURL != "" {
			addRemoteStorage(influxdb.NewClient(cfg.influxdbURL, cfg.remoteStorageTimeout, cfg.influxdbDatabase, cfg.influxdbRetentionPolicy))
		}

		sampleAppender = fanout
	}

	var (
		notificationHandler = notification.NewNotificationHandler(&cfg.notification)
		targetManager       = retrieval.NewTargetManager(sampleAppender)
		queryEngine         = promql.NewEngine(memStorage, &cfg.queryEngine)
	)

	ruleManager := rules.NewManager(&rules.ManagerOptions{
		SampleAppender:      sampleAppender,
		NotificationHandler: notificationHandler,
		QueryEngine:         queryEngine,
		PrometheusURL:       cfg.prometheusURL,
		PathPrefix:          cfg.web.PathPrefix,
	})

	flags := map[string]string{}
	cfg.fs.VisitAll(func(f *flag.Flag) {
		flags[f.Name] = f.Value.String()
	})

	status := &web.PrometheusStatus{
		BuildInfo:   BuildInfo,
		TargetPools: targetManager.Pools,
		Rules:       ruleManager.Rules,
		Flags:       flags,
		Birth:       time.Now(),
	}

	webHandler := web.New(memStorage, queryEngine, ruleManager, status, &cfg.web)

	if !reloadConfig(cfg.configFile, status, targetManager, ruleManager) {
		os.Exit(1)
	}

	// Wait for reload or termination signals. Start the handler for SIGHUP as
	// early as possible, but ignore it until we are ready to handle reloading
	// our config.
	hup := make(chan os.Signal)
	hupReady := make(chan bool)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		<-hupReady
		for range hup {
			reloadConfig(cfg.configFile, status, targetManager, ruleManager)
		}
	}()

	// Start all components.
	if err := memStorage.Start(); err != nil {
		log.Errorln("Error opening memory series storage:", err)
		return 1
	}
	defer func() {
		if err := memStorage.Stop(); err != nil {
			log.Errorln("Error stopping storage:", err)
		}
	}()

	// The storage has to be fully initialized before registering.
	registry.MustRegister(memStorage)
	registry.MustRegister(notificationHandler)

	for _, q := range remoteStorageQueues {
		registry.MustRegister(q)

		go q.Run()
		defer q.Stop()
	}

	go ruleManager.Run()
	defer ruleManager.Stop()

	go notificationHandler.Run()
	defer notificationHandler.Stop()

	go targetManager.Run()
	defer targetManager.Stop()

	defer queryEngine.Stop()

	go webHandler.Run()

	// Wait for reload or termination signals.
	close(hupReady) // Unblock SIGHUP handler.

	term := make(chan os.Signal)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)
	select {
	case <-term:
		log.Warn("Received SIGTERM, exiting gracefully...")
	case <-webHandler.Quit():
		log.Warn("Received termination request via web service, exiting gracefully...")
	}

	close(hup)

	log.Info("See you next time!")
	return 0
}

// Reloadable things can change their internal state to match a new config
// and handle failure gracefully.
type Reloadable interface {
	ApplyConfig(*config.Config) bool
}

func reloadConfig(filename string, rls ...Reloadable) bool {
	log.Infof("Loading configuration file %s", filename)

	conf, err := config.LoadFromFile(filename)
	if err != nil {
		log.Errorf("Couldn't load configuration (-config.file=%s): %v", filename, err)
		log.Errorf("Note: The configuration format has changed with version 0.14. Please see the documentation (http://prometheus.io/docs/operating/configuration/) and the provided configuration migration tool (https://github.com/prometheus/migrate).")
		return false
	}
	success := true

	for _, rl := range rls {
		success = success && rl.ApplyConfig(conf)
	}
	return success
}