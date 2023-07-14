package pkg

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/xhit/go-str2duration"
)

const (
	viperLogLevel  = "log_level"
	viperPort      = "port"
	viperRGWURL    = "rgw_url"
	viperInterval  = "interval"
	viperAccessKey = "access_key"
	viperSecretKey = "secret_key"
)

func RunServer() (*logrus.Logger, error) {
	// Initialize viper
	v := viper.New()
	v.SetEnvPrefix("RGW_EXPORTER")

	// Initialize the input defaults
	v.SetDefault(viperLogLevel, logrus.InfoLevel.String())
	v.SetDefault(viperPort, 8080)
	v.SetDefault(viperRGWURL, "")
	v.SetDefault(viperInterval, "1m")
	v.SetDefault(viperAccessKey, "")
	v.SetDefault(viperSecretKey, "")

	// Read them from ENV
	v.AutomaticEnv()

	// Create a logger
	log := logrus.New()
	logLevelStr := v.GetString(viperLogLevel)
	logLevel, err := logrus.ParseLevel(logLevelStr)
	if err != nil {
		return log, fmt.Errorf("failed to parse RGW_EXPORTER_CEPH_URL `%s` - %w", logLevelStr, err)
	}

	log.SetLevel(logLevel)

	// Validate the inputs
	rgwURLStr := v.GetString(viperRGWURL)
	if rgwURLStr == "" {
		return log, fmt.Errorf("RGW_EXPORTER_CEPH_URL is a required argument")
	}

	rgwURL, err := url.Parse(rgwURLStr)
	if err != nil {
		return log, fmt.Errorf("failed to parse RGW_EXPORTER_CEPH_URL `%s` - %w", rgwURLStr, err)
	}

	intervalStr := v.GetString(viperInterval)
	if intervalStr == "" {
		return log, fmt.Errorf("RGW_EXPORTER_INTERVAL is a required argument")
	}

	interval, err := str2duration.Str2Duration(intervalStr)
	if err != nil {
		return log, fmt.Errorf("failed to parse RGW_EXPORTER_INTERVAL `%s` as a duration - %w", intervalStr, err)
	}

	accessKey := v.GetString(viperAccessKey)
	if accessKey == "" {
		return log, fmt.Errorf("RGW_EXPORTER_ACCESS_KEY is a required argument")
	}

	secretKey := v.GetString(viperSecretKey)
	if secretKey == "" {
		return log, fmt.Errorf("RGW_EXPORTER_SECRET_KEY is a required argument")
	}

	// Start the server
	serverCtx, serverCancel := context.WithCancel(context.Background())

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	srv, err := startServer(serverCtx, log, rgwURL, accessKey, secretKey, v.GetInt(viperPort), interval)
	if err != nil {
		serverCancel()
		return log, fmt.Errorf("failed to start server - %w", err)
	}

	// Wait for a signal to shutdown
	doneSignal := <-done
	log.Debugf("Got %v signal", doneSignal)

	timeout := 10
	log.Infof("HTTP Server Stopping... Waiting up to %v seconds for all in-progress requests to finish.", timeout)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("HTTP Server Shutdown Failed - %+v", err)
	}
	// Close as well in case of context deadline - so we ensure connections are closed.
	if err := srv.Close(); err != nil {
		log.Errorf("Failed closing server: %v", err)
	}

	serverCancel()

	delay := 2
	log.Infof("HTTP Server shutdown finished - will now delay %d seconds before exiting", delay)
	time.Sleep(time.Duration(delay) * time.Second)
	log.Info("Server Exited")

	return log, nil
}

func startServer(ctx context.Context, log *logrus.Logger, rgwURL *url.URL, accessKey string, secretKey string, port int, scrapeInterval time.Duration) (*http.Server, error) {
	// Create a http client to use for requests
	client := makeHTTPClient()

	// Create the S3 credentials
	creds := credentials.NewStaticCredentials(accessKey, secretKey, "")

	// Create the metrics instance and start it scraping
	metrics := NewRGWMetrics()
	metrics.StartScraping(ctx, log, client, rgwURL, creds, scrapeInterval)

	// Finally create and start the server
	srv := &http.Server{
		Addr:        fmt.Sprintf(":%d", port),
		Handler:     createRouter(log, client, rgwURL, metrics),
		BaseContext: func(_ net.Listener) context.Context { return ctx },
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorf("Failed to start listening server - %v", err)
		}
	}()

	log.Info("HTTP Server Started")
	return srv, nil
}

func makeHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       10 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Forbid all redirects. Redirect is only explicitly allowed for GET / HEAD requests, and we do many other types of requests
			//
			// * HTTP Spec:
			// * If the 301 status code is received in response to a request other than GET or HEAD, the user agent MUST NOT automatically
			// * redirect the request unless it can be confirmed by the user, since this might change the conditions under which the request was issued.
			//
			// Therefore, any redirects of non GET/HEAD requests are undefined behavior.
			// By forbidding all redirects, this also makes it easier to detect if the user accidentally typed http:// instead of https://
			return http.ErrUseLastResponse
		},
	}
}

func createRouter(log *logrus.Logger, client *http.Client, rgwURL *url.URL, metrics *RGWMetrics) http.Handler {
	router := http.NewServeMux()

	// Add the health check handlers
	// These are used by systems like kubernetes to check if the container is still alive and well
	router.HandleFunc("/readiness", func(w http.ResponseWriter, r *http.Request) {
		// Readiness determines if the service is *actually* able to serve real data
		// So we check the health of our connection to ceph, and if that passes
		// we return 200

		if err := cephHealthCheck(r.Context(), client, rgwURL); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			if _, err := w.Write([]byte(fmt.Sprintf("Ceph Health check failed - %v", err))); err != nil {
				log.Errorf("Failed writing readiness failure - %v", err)
			}
		} else {
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte("ready")); err != nil {
				log.Errorf("Failed writing readiness success - %v", err)
			}
		}
	})
	router.HandleFunc("/liveness", func(w http.ResponseWriter, r *http.Request) {
		// Liveness determines if the service is able to make forward progress at all
		// AKA, is the process hung
		// So we just return 200, no matter what. If the checking service is able to get our
		// response, then our server stack is at least able to make forward progress

		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("alive")); err != nil {
			log.Errorf("Failed writing liveness success - %v", err)
		}
	})

	// Add the main metrics handler
	router.Handle("/metrics", metrics.Handler())

	return router
}

func cephHealthCheck(ctx context.Context, client *http.Client, rgwURL *url.URL) error {
	cephHealthcheckURL, err := rgwURL.Parse("swift/healthcheck")
	if err != nil {
		return fmt.Errorf("failed to create ceph healthcheck URL - %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", cephHealthcheckURL.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create request - %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to query ceph - %w", err)
	}

	_, err = io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()

	if err != nil {
		return fmt.Errorf("failed to read ceph resp body - %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("failed to close ceph resp body - %w", closeErr)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request to ceph returned %d", resp.StatusCode)
	}

	return nil
}
