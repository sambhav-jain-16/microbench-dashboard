// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package app implements the performance data analysis server.
package app

import (
	"net/http"
)

// App manages the analysis server logic.
// Construct an App instance and call RegisterOnMux to connect it with an HTTP server.
type App struct {
	// BaseDir is the directory containing the "template" directory.
	// If empty, the current directory will be used.
	BaseDir string

	// VictoriaMetricsURL is the URL of the VictoriaMetrics server.
	VictoriaMetricsURL string

	// AuthCronEmail is the service account email which requests to
	// /cron/syncinflux must contain an OICD authentication token for, with
	// audience "/cron/syncinflux".
	//
	// If empty, no authentication is required.
	AuthCronEmail string
}

// RegisterOnMux registers the app's URLs on mux.
func (a *App) RegisterOnMux(mux *http.ServeMux) {
	a.dashboardRegisterOnMux(mux)
}
