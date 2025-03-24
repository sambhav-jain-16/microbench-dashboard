// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// perf runs an HTTP server for benchmark analysis.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"

	"golang.org/x/build/internal/https"
	"golang.org/x/build/perf/app"
)

var (
	victoriaMetricsURL = flag.String("victoriametrics-url", os.Getenv("VICTORIAMETRICS_URL"), "URL of the VictoriaMetrics server")
	authCronEmail      = flag.String("auth-cron-email", "", "If set, requests to /cron/syncinflux must be authenticated as the passed service account.")
)

func main() {
	https.RegisterFlags(flag.CommandLine)
	flag.Parse()

	app := &app.App{
		VictoriaMetricsURL: *victoriaMetricsURL,
		AuthCronEmail:      *authCronEmail,
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.RedirectHandler("dashboard/", 307))
	app.RegisterOnMux(mux)

	log.Printf("Serving...")

	ctx := context.Background()
	log.Fatal(https.ListenAndServe(ctx, mux))
}
