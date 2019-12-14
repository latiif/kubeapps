package main

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/kubeapps/kubeapps/cmd/kubeops/internal/handler"
	"github.com/kubeapps/kubeapps/pkg/agent"
	"github.com/kubeapps/kubeapps/pkg/auth"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/urfave/negroni"
	"k8s.io/helm/pkg/helm/environment"
)

const driverEnvVar = "HELM_DRIVER"
const defaultHelmDriver agent.DriverType = agent.Secret

var (
	settings         environment.EnvSettings
	chartsvcURL      string
	helmDriverArg    string
	userAgentComment string
	listLimit        int
	timeout          int64
)

func init() {
	settings.AddFlags(pflag.CommandLine) // necessary???
	pflag.StringVar(&chartsvcURL, "chartsvc-url", "https://kubeapps-internal-chartsvc:8080", "URL to the internal chartsvc")
	pflag.StringVar(&helmDriverArg, "helm-driver", "", "which Helm driver type to use")
	pflag.IntVar(&listLimit, "list-max", 256, "maximum number of releases to fetch")
	pflag.StringVar(&userAgentComment, "user-agent-comment", "", "UserAgent comment used during outbound requests") // necessary???
	// Default timeout from https://github.com/helm/helm/blob/b0b0accdfc84e154b3d48ec334cd5b4f9b345667/cmd/helm/install.go#L216
	pflag.Int64Var(&timeout, "timeout", 300, "Timeout to perform release operations (install, upgrade, rollback, delete)")
}

func main() {
	pflag.Parse()
	settings.Init(pflag.CommandLine)

	options := agent.Options{
		ListLimit: listLimit,
		Timeout:   timeout,
	}

	// Will panic below if an invalid driver type is provided.
	driverType := defaultHelmDriver
	if helmDriverArg != "" {
		driverType = agent.ParseDriverType(helmDriverArg)
	} else {
		// CLI argument was not provided; check environment variable.
		helmDriverEnv := os.Getenv(driverEnvVar)
		if helmDriverEnv != "" {
			driverType = agent.ParseDriverType(helmDriverEnv)
		}
	}
	withAgentConfig := handler.WithAgentConfig(driverType, options)
	r := mux.NewRouter()

	// Routes
	// Auth not necessary here with Helm 3 because it's done by Kubernetes.
	apiv1 := r.PathPrefix("/v1").Subrouter()
	apiv1.Methods("GET").Path("/releases").Handler(negroni.New(
		negroni.Wrap(withAgentConfig(handler.ListAllReleases)),
	))
	apiv1.Methods("GET").Path("/namespaces/{namespace}/releases").Handler(negroni.New(
		negroni.Wrap(withAgentConfig(handler.ListReleases)),
	))
	apiv1.Methods("POST").Path("/namespaces/{namespace}/releases").Handler(negroni.New(
		negroni.Wrap(withAgentConfig(handler.CreateRelease)),
	))

	// Chartsvc reverse proxy
	authGate := auth.AuthGate()
	parsedChartsvcURL, err := url.Parse(chartsvcURL)
	if err != nil {
		log.Fatalf("Unable to parse the chartsvc URL: %v", err)
	}
	chartsvcProxy := httputil.NewSingleHostReverseProxy(parsedChartsvcURL)
	chartsvcPrefix := "/chartsvc"
	chartsvcRouter := r.PathPrefix(chartsvcPrefix).Subrouter()
	// Logos don't require authentication so bypass that step
	chartsvcRouter.Methods("GET").Path("/v1/assets/{repo}/{id}/logo").Handler(negroni.New(
		negroni.Wrap(http.StripPrefix(chartsvcPrefix, chartsvcProxy)),
	))
	chartsvcRouter.Methods("GET").Handler(negroni.New(
		authGate,
		negroni.Wrap(http.StripPrefix(chartsvcPrefix, chartsvcProxy)),
	))

	n := negroni.Classic()
	n.UseHandler(r)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port

	srv := &http.Server{
		Addr:    addr,
		Handler: n,
	}

	go func() {
		log.WithFields(log.Fields{"addr": addr}).Info("Started Kubeops")
		err := srv.ListenAndServe()
		if err != nil {
			log.Info(err)
		}
	}()

	// Catch SIGINT and SIGTERM
	// Set up channel on which to send signal notifications.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	log.Debug("Set system to get notified on signals")
	s := <-c
	log.Infof("Received signal: %v. Waiting for existing requests to finish", s)
	// Set a timeout value high enough to let k8s terminationGracePeriodSeconds to act
	// accordingly and send a SIGKILL if needed
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3600)
	defer cancel()
	// Doesn't block if no connections, but will otherwise wait
	// until the timeout deadline.
	srv.Shutdown(ctx)
	log.Info("All requests have been served. Exiting")
	os.Exit(0)
}
