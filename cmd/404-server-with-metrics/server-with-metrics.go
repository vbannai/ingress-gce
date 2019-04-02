/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// A webserver that only serves a 404 page. Used as a default backend for ingress gce object for kubernetes cluster.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// command line flags/arguments
	port              = flag.Int("port", 8080, "Port number to serve default backend 404 page.")
	serverTimeout     = flag.Duration("timeout", 5*time.Second, "Time in seconds to wait before forcefully terminating the server.")
	readTimeout       = flag.Duration("read timeout", 10*time.Second, "Time in seconds to read the entire request before timing out.")
	readHeaderTimeout = flag.Duration("read header timeout", 10*time.Second, "Time in seconds to read the request header before timing out.")
	writeTimeout      = flag.Duration("write timeout", 10*time.Second, "Time in seconds to write response before timing out.")
	idleTimeout       = flag.Duration("idle timeout", 10*time.Second, "Time in seconds to wait for the next request when keep-alives are enabled.")
	maxJobs           = flag.Int("max workers", 100, "Number of parallel/concurrent jobs to run.")
	idleLogTimer      = flag.Duration("idle log timeout", 1*time.Hour, "Timer for keep alive logger")
	logSampleRequests = flag.Float64("log percent of sample requests", 0.0, "Sample http requests to log [0.0 to 1.0]")
)

func main() {
	flag.Parse()
	hostName, err := os.Hostname()
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not get the hostname: %v\n", err)
	}

	server := newServer(hostName, *port)
	server.registerHandlers()
	fmt.Fprintf(os.Stderr, "Default 404 server is running with GOMAXPROCS(%d) on %s:%d\n", runtime.GOMAXPROCS(-1), hostName, *port)

	go func() {
		err := server.httpServer.ListenAndServe()
		if err != nil {
			switch err {
			case http.ErrServerClosed:
				fmt.Fprintf(os.Stderr, "server shutting down or received shutdown: %v\n", err)
				os.Exit(0)
			case http.ErrHandlerTimeout:
				fmt.Fprintf(os.Stderr, "handler timedout: %v\n", err)
			default:
				// Should we Fatal() ?
				fmt.Fprintf(os.Stderr, "could not start http server or internal error: %v\n", err)
			}
		}
	}()

	// go function for monitoring idle time and logging keep alive messages
	go func() {
		for {
			select {
			case <-server.idleChannel:
			case <-time.After(*idleLogTimer):
				fmt.Fprintf(os.Stderr, "No connection requests received for 1 hour\n")
			}
		}
	}()

	gracefulShutdown(server)
}

// server encompasses the shared data for the default HTTP server
type server struct {
	// totalRequests is a  prometheus vector counter for tracking total http requests
	totalRequests *prometheus.CounterVec
	// requestDuration is a prometheus vector histogram for tracking duration time for requests
	requestDuration *prometheus.HistogramVec
	// httpServer is a private pointer to the http.Server
	httpServer *http.Server
	// mux is a pointer to the ServerMux
	mux *http.ServeMux
	// context used to signal cancel for shutdown and interrupts
	ctx context.Context
	// cancel function for the context
	cancel context.CancelFunc
	// idle channel for monitoring activity on the server
	idleChannel chan bool
}

// newServer returns server that implements the http.Handler interface
func newServer(hostName string, port int) *server {
	s := &server{
		httpServer: &http.Server{
			Addr:              fmt.Sprintf(":%d", port),
			ReadTimeout:       *readTimeout,
			ReadHeaderTimeout: *readHeaderTimeout,
			WriteTimeout:      *writeTimeout,
			IdleTimeout:       *idleTimeout,
		},
	}

	// create http request counter with the labels as follows
	//    "handler" --> handler used for the uri path
	//    "method" --> http request method (GET, POST, ...)
	s.totalRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_404_request_total",
			Help: "Total 404 requests received by the default HTTP server",
		},
		[]string{"rule", "method"})
	prometheus.MustRegister(s.totalRequests)

	// create http request processing duration histogram vector with the labels
	//    "method" --> http request method (GET, POST, ...)
	s.requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_404_request_duration_ms",
			Help: "Duration of the http request handling in ms",
			// Need a SLO for the bucket values
			Buckets: []float64{0.5, 1.0, 2.0},
		},
		[]string{"method"},
	)
	prometheus.MustRegister(s.requestDuration)

	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.idleChannel = make(chan bool)

	return s
}

// registerHandlers registers the callbacks for the various URIs supported by the default HTTP server.
func (s *server) registerHandlers() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.notFoundHandler())
	mux.HandleFunc("/shutdown", s.shutdownHandler())
	mux.Handle("/metrics", promhttp.Handler())

	s.mux = mux
	s.httpServer.Handler = mux
}

// shutdown handler handles the graceful shutdown of the server
func (s *server) shutdownHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "got shutdown request, shutting down \n")
		s.cancel()
	}
}

// notFoundHandler uses the default http NotFoundHandler which returns a 404 status code
func (s *server) notFoundHandler() http.HandlerFunc {
	rand.Seed(1)
	return func(w http.ResponseWriter, r *http.Request) {
		// compute the duration of handling the request
		dt := prometheus.NewTimer(prometheus.ObserverFunc(func(value float64) {
			s.requestDuration.WithLabelValues(r.Method).Observe(value * 1000.0)
		}))
		defer dt.ObserveDuration()

		// Get the registered pattern that matches the request
		_, pattern := s.mux.Handler(r)
		// Increment the totalRequests counter with the HTTP method label
		s.totalRequests.WithLabelValues(pattern, r.Method).Inc()

		path := r.URL.Path
		w.WriteHeader(http.StatusNotFound)
		// We log 1 out of 4 requests to the logs (make it configurable by a flag??)
		fmt.Fprintf(w, "reached NotFound backend, service rules not setup correctly for %s \n", path)
		s.idleChannel <- true
		if rand.Float64() < *logSampleRequests {
			fmt.Fprintf(os.Stderr, "reached NotFound backend, service rules not setup correctly for %s \n", path)
		}
	}
}

// graceful shutdown handler
func gracefulShutdown(s *server) {
	// have a small buffered channel so as not to lose signal sent when we were not ready.
	c := make(chan os.Signal, 1)
	defer close(c)
	signal.Notify(c, os.Interrupt)

	select {
	case interrupt := <-c:
		fmt.Fprintf(os.Stderr, "received interrupt, doing a graceful shutdown: %v \n", interrupt)
	case <-s.ctx.Done():
		fmt.Fprintf(os.Stderr, "received /shutdown message, doing a graceful shutdown: \n")
	}

	s.httpServer.Shutdown(context.Background())
}
