package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/cloudfoundry/bosh-cli/director"
	"github.com/cloudfoundry/bosh-cli/uaa"
	"github.com/cloudfoundry/bosh-utils/logger"
	"github.com/cloudfoundry/bosh-utils/system"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/bosh-prometheus/bosh_exporter/collectors"
	"github.com/bosh-prometheus/bosh_exporter/deployments"
	"github.com/bosh-prometheus/bosh_exporter/filters"
)

var (
	boshURL = kingpin.Flag(
		"bosh.url", "BOSH URL ($BOSH_EXPORTER_BOSH_URL)",
	).Envar("BOSH_EXPORTER_BOSH_URL").Required().String()

	boshUsername = kingpin.Flag(
		"bosh.username", "BOSH Username ($BOSH_EXPORTER_BOSH_USERNAME)",
	).Envar("BOSH_EXPORTER_BOSH_USERNAME").String()

	boshPassword = kingpin.Flag(
		"bosh.password", "BOSH Password ($BOSH_EXPORTER_BOSH_PASSWORD)",
	).Envar("BOSH_EXPORTER_BOSH_PASSWORD").String()

	boshUAAClientID = kingpin.Flag(
		"bosh.uaa.client-id", "BOSH UAA Client ID ($BOSH_EXPORTER_BOSH_UAA_CLIENT_ID)",
	).Envar("BOSH_EXPORTER_BOSH_UAA_CLIENT_ID").String()

	boshUAAClientSecret = kingpin.Flag(
		"bosh.uaa.client-secret", "BOSH UAA Client Secret ($BOSH_EXPORTER_BOSH_UAA_CLIENT_SECRET)",
	).Envar("BOSH_EXPORTER_BOSH_UAA_CLIENT_SECRET").String()

	boshLogLevel = kingpin.Flag(
		"bosh.log-level", "BOSH Log Level ($BOSH_EXPORTER_BOSH_LOG_LEVEL)",
	).Envar("BOSH_EXPORTER_BOSH_LOG_LEVEL").Default("ERROR").String()

	boshCACertFile = kingpin.Flag(
		"bosh.ca-cert-file", "BOSH CA Certificate file ($BOSH_EXPORTER_BOSH_CA_CERT_FILE)",
	).Envar("BOSH_EXPORTER_BOSH_CA_CERT_FILE").Required().ExistingFile()

	filterDeployments = kingpin.Flag(
		"filter.deployments", "Comma separated deployments to filter ($BOSH_EXPORTER_FILTER_DEPLOYMENTS)",
	).Envar("BOSH_EXPORTER_FILTER_DEPLOYMENTS").Default("").String()

	filterAZs = kingpin.Flag(
		"filter.azs", "Comma separated AZs to filter ($BOSH_EXPORTER_FILTER_AZS)",
	).Envar("BOSH_EXPORTER_FILTER_AZS").Default("").String()

	filterCollectors = kingpin.Flag(
		"filter.collectors", "Comma separated collectors to filter (Deployments,Jobs,ServiceDiscovery) ($BOSH_EXPORTER_FILTER_COLLECTORS)",
	).Envar("BOSH_EXPORTER_FILTER_COLLECTORS").Default("").String()

	metricsNamespace = kingpin.Flag(
		"metrics.namespace", "Metrics Namespace ($BOSH_EXPORTER_METRICS_NAMESPACE)",
	).Envar("BOSH_EXPORTER_METRICS_NAMESPACE").Default("bosh").String()

	metricsEnvironment = kingpin.Flag(
		"metrics.environment", "Environment label to be attached to metrics ($BOSH_EXPORTER_METRICS_ENVIRONMENT)",
	).Envar("BOSH_EXPORTER_METRICS_ENVIRONMENT").Required().String()

	sdFilename = kingpin.Flag(
		"sd.filename", "Full path to the Service Discovery output file ($BOSH_EXPORTER_SD_FILENAME)",
	).Envar("BOSH_EXPORTER_SD_FILENAME").Default("bosh_target_groups.json").String()

	sdProcessesRegexp = kingpin.Flag(
		"sd.processes_regexp", "Regexp to filter Service Discovery processes names ($BOSH_EXPORTER_SD_PROCESSES_REGEXP)",
	).Envar("BOSH_EXPORTER_SD_PROCESSES_REGEXP").Default("").String()

	listenAddress = kingpin.Flag(
		"web.listen-address", "Address to listen on for web interface and telemetry ($BOSH_EXPORTER_WEB_LISTEN_ADDRESS)",
	).Envar("BOSH_EXPORTER_WEB_LISTEN_ADDRESS").Default(":9190").String()

	metricsPath = kingpin.Flag(
		"web.telemetry-path", "Path under which to expose Prometheus metrics ($BOSH_EXPORTER_WEB_TELEMETRY_PATH)",
	).Envar("BOSH_EXPORTER_WEB_TELEMETRY_PATH").Default("/metrics").String()

	authUsername = kingpin.Flag(
		"web.auth.username", "Username for web interface basic auth ($BOSH_EXPORTER_WEB_AUTH_USERNAME)",
	).Envar("BOSH_EXPORTER_WEB_AUTH_USERNAME").String()

	authPassword = kingpin.Flag(
		"web.auth.password", "Password for web interface basic auth ($BOSH_EXPORTER_WEB_AUTH_PASSWORD)",
	).Envar("BOSH_EXPORTER_WEB_AUTH_PASSWORD").String()

	tlsCertFile = kingpin.Flag(
		"web.tls.cert_file", "Path to a file that contains the TLS certificate (PEM format). If the certificate is signed by a certificate authority, the file should be the concatenation of the server's certificate, any intermediates, and the CA's certificate ($BOSH_EXPORTER_WEB_TLS_CERTFILE)",
	).Envar("BOSH_EXPORTER_WEB_TLS_CERTFILE").ExistingFile()

	tlsKeyFile = kingpin.Flag(
		"web.tls.key_file", "Path to a file that contains the TLS private key (PEM format) ($BOSH_EXPORTER_WEB_TLS_KEYFILE)",
	).Envar("BOSH_EXPORTER_WEB_TLS_KEYFILE").ExistingFile()
)

func init() {
	prometheus.MustRegister(version.NewCollector(*metricsNamespace))
}

type basicAuthHandler struct {
	handler  http.HandlerFunc
	username string
	password string
}

func (h *basicAuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	username, password, ok := r.BasicAuth()
	if !ok || username != h.username || password != h.password {
		log.Errorf("Invalid HTTP auth from `%s`", r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", "Basic realm=\"metrics\"")
		http.Error(w, "Invalid username or password", http.StatusUnauthorized)
		return
	}
	h.handler(w, r)
	return
}

func prometheusHandler() http.Handler {
	handler := prometheus.Handler()

	if *authUsername != "" && *authPassword != "" {
		handler = &basicAuthHandler{
			handler:  prometheus.Handler().ServeHTTP,
			username: *authUsername,
			password: *authPassword,
		}
	}

	return handler
}

func readCACert(CACertFile string, logger logger.Logger) (string, error) {
	if CACertFile != "" {
		fs := system.NewOsFileSystem(logger)

		CACertFileFullPath, err := fs.ExpandPath(CACertFile)
		if err != nil {
			return "", err
		}

		CACert, err := fs.ReadFileString(CACertFileFullPath)
		if err != nil {
			return "", err
		}

		return CACert, nil
	}

	return "", nil
}

func buildBOSHClient() (director.Director, error) {
	logLevel, err := logger.Levelify(*boshLogLevel)
	if err != nil {
		return nil, err
	}

	logger := logger.NewLogger(logLevel)

	directorConfig, err := director.NewConfigFromURL(*boshURL)
	if err != nil {
		return nil, err
	}

	boshCACert, err := readCACert(*boshCACertFile, logger)
	if err != nil {
		return nil, err
	}
	directorConfig.CACert = boshCACert

	anonymousDirector, err := director.NewFactory(logger).New(directorConfig, nil, nil)
	if err != nil {
		return nil, err
	}

	boshInfo, err := anonymousDirector.Info()
	if err != nil {
		return nil, err
	}

	if boshInfo.Auth.Type != "uaa" {
		directorConfig.Client = *boshUsername
		directorConfig.ClientSecret = *boshPassword
	} else {
		uaaURL := boshInfo.Auth.Options["url"]
		uaaURLStr, ok := uaaURL.(string)
		if !ok {
			return nil, errors.New(fmt.Sprintf("Expected UAA URL '%s' to be a string", uaaURL))
		}

		uaaConfig, err := uaa.NewConfigFromURL(uaaURLStr)
		if err != nil {
			return nil, err
		}

		uaaConfig.CACert = boshCACert

		if *boshUAAClientID != "" && *boshUAAClientSecret != "" {
			uaaConfig.Client = *boshUAAClientID
			uaaConfig.ClientSecret = *boshUAAClientSecret
		} else {
			uaaConfig.Client = "bosh_cli"
		}

		uaaFactory := uaa.NewFactory(logger)
		uaaClient, err := uaaFactory.New(uaaConfig)
		if err != nil {
			return nil, err
		}

		if *boshUAAClientID != "" && *boshUAAClientSecret != "" {
			directorConfig.TokenFunc = uaa.NewClientTokenSession(uaaClient).TokenFunc
		} else {
			answers := []uaa.PromptAnswer{
				uaa.PromptAnswer{
					Key:   "username",
					Value: *boshUsername,
				},
				uaa.PromptAnswer{
					Key:   "password",
					Value: *boshPassword,
				},
			}
			accessToken, err := uaaClient.OwnerPasswordCredentialsGrant(answers)
			if err != nil {
				return nil, err
			}

			origToken := uaaClient.NewStaleAccessToken(accessToken.RefreshToken().Value())
			directorConfig.TokenFunc = uaa.NewAccessTokenSession(origToken).TokenFunc
		}
	}

	boshFactory := director.NewFactory(logger)
	boshClient, err := boshFactory.New(directorConfig, director.NewNoopTaskReporter(), director.NewNoopFileReporter())
	if err != nil {
		return nil, err
	}

	return boshClient, nil
}

func main() {
	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print("fbosh_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	log.Infoln("Starting bosh_exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	boshClient, err := buildBOSHClient()
	if err != nil {
		log.Errorf("Error creating BOSH Client: %s", err.Error())
		os.Exit(1)
	}

	boshInfo, err := boshClient.Info()
	if err != nil {
		log.Errorf("Error reading BOSH Info: %s", err.Error())
		os.Exit(1)
	}
	log.Infof("Using BOSH Director `%s` (%s)", boshInfo.Name, boshInfo.UUID)

	var deploymentsFilters []string
	if *filterDeployments != "" {
		deploymentsFilters = strings.Split(*filterDeployments, ",")
	}
	deploymentsFilter := filters.NewDeploymentsFilter(deploymentsFilters, boshClient)
	deploymentsFetcher := deployments.NewFetcher(*deploymentsFilter)

	var azsFilters []string
	if *filterAZs != "" {
		azsFilters = strings.Split(*filterAZs, ",")
	}
	azsFilter := filters.NewAZsFilter(azsFilters)

	var collectorsFilters []string
	if *filterCollectors != "" {
		collectorsFilters = strings.Split(*filterCollectors, ",")
	}
	collectorsFilter, err := filters.NewCollectorsFilter(collectorsFilters)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}

	var processesFilters []string
	if *sdProcessesRegexp != "" {
		processesFilters = []string{*sdProcessesRegexp}
	}
	processesFilter, err := filters.NewRegexpFilter(processesFilters)
	if err != nil {
		log.Errorf("Error processing Processes Regexp: %v", err)
		os.Exit(1)
	}

	boshCollector := collectors.NewBoshCollector(
		*metricsNamespace,
		*metricsEnvironment,
		boshInfo.Name,
		boshInfo.UUID,
		*sdFilename,
		deploymentsFetcher,
		collectorsFilter,
		azsFilter,
		processesFilter,
	)
	prometheus.MustRegister(boshCollector)

	http.Handle(*metricsPath, prometheusHandler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>BOSH Exporter</title></head>
             <body>
             <h1>BOSH Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})

	if *tlsCertFile != "" && *tlsKeyFile != "" {
		log.Infoln("Listening TLS on", *listenAddress)
		log.Fatal(http.ListenAndServeTLS(*listenAddress, *tlsCertFile, *tlsKeyFile, nil))
	} else {
		log.Infoln("Listening on", *listenAddress)
		log.Fatal(http.ListenAndServe(*listenAddress, nil))
	}
}
