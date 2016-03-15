package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/cf-debug-server"
	cf_lager "github.com/cloudfoundry-incubator/cf-lager"
	"github.com/cloudfoundry-incubator/cf_http"
	"github.com/cloudfoundry-incubator/consuladapter"
	"github.com/cloudfoundry-incubator/executor"
	executorinit "github.com/cloudfoundry-incubator/executor/initializer"
	"github.com/cloudfoundry-incubator/locket"
	"github.com/cloudfoundry-incubator/rep"
	"github.com/cloudfoundry-incubator/rep/auction_cell_rep"
	"github.com/cloudfoundry-incubator/rep/evacuation"
	"github.com/cloudfoundry-incubator/rep/evacuation/evacuation_context"
	"github.com/cloudfoundry-incubator/rep/generator"
	"github.com/cloudfoundry-incubator/rep/handlers"
	"github.com/cloudfoundry-incubator/rep/harmonizer"
	"github.com/cloudfoundry-incubator/rep/maintain"
	"github.com/cloudfoundry/dropsonde"
	"github.com/nu7hatch/gouuid"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/operationq"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/ifrit/sigmon"
	"github.com/tedsuo/rata"
)

var sessionName = flag.String(
	"sessionName",
	"rep",
	"consul session name",
)

var consulCluster = flag.String(
	"consulCluster",
	"",
	"comma-separated list of consul server URLs (scheme://ip:port)",
)

var lockTTL = flag.Duration(
	"lockTTL",
	locket.LockTTL,
	"TTL for service lock",
)

var lockRetryInterval = flag.Duration(
	"lockRetryInterval",
	locket.RetryInterval,
	"interval to wait before retrying a failed lock acquisition",
)

var listenAddr = flag.String(
	"listenAddr",
	"0.0.0.0:1800",
	"host:port to serve auction and LRP stop requests on",
)

var cellID = flag.String(
	"cellID",
	"",
	"the ID used by the rep to identify itself to external systems - must be specified",
)

var zone = flag.String(
	"zone",
	"",
	"the availability zone associated with the rep",
)

var pollingInterval = flag.Duration(
	"pollingInterval",
	30*time.Second,
	"the interval on which to scan the executor",
)

var dropsondePort = flag.Int(
	"dropsondePort",
	3457,
	"port the local metron agent is listening on",
)

var communicationTimeout = flag.Duration(
	"communicationTimeout",
	10*time.Second,
	"Timeout applied to all HTTP requests.",
)

var evacuationTimeout = flag.Duration(
	"evacuationTimeout",
	10*time.Minute,
	"Timeout to wait for evacuation to complete",
)

var evacuationPollingInterval = flag.Duration(
	"evacuationPollingInterval",
	10*time.Second,
	"the interval on which to scan the executor during evacuation",
)

var bbsAddress = flag.String(
	"bbsAddress",
	"",
	"Address to the BBS Server",
)

var bbsCACert = flag.String(
	"bbsCACert",
	"",
	"path to certificate authority cert used for mutually authenticated TLS BBS communication",
)

var bbsClientCert = flag.String(
	"bbsClientCert",
	"",
	"path to client cert used for mutually authenticated TLS BBS communication",
)

var bbsClientKey = flag.String(
	"bbsClientKey",
	"",
	"path to client key used for mutually authenticated TLS BBS communication",
)

var bbsClientSessionCacheSize = flag.Int(
	"bbsClientSessionCacheSize",
	0,
	"Capacity of the ClientSessionCache option on the TLS configuration. If zero, golang's default will be used",
)

var bbsMaxIdleConnsPerHost = flag.Int(
	"bbsMaxIdleConnsPerHost",
	0,
	"Controls the maximum number of idle (keep-alive) connctions per host. If zero, golang's default will be used",
)

type stackPathMap rep.StackPathMap

func (s *stackPathMap) String() string {
	return fmt.Sprintf("%v", *s)
}

func (s *stackPathMap) Set(value string) error {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return errors.New("Invalid preloaded RootFS value: not of the form 'stack-name:path'")
	}

	if parts[0] == "" {
		return errors.New("Invalid preloaded RootFS value: blank stack")
	}

	if parts[1] == "" {
		return errors.New("Invalid preloaded RootFS value: blank path")
	}

	(*s)[parts[0]] = parts[1]
	return nil
}

type providers []string

func (p *providers) String() string {
	return fmt.Sprintf("%v", *p)
}

func (p *providers) Set(value string) error {
	if value == "" {
		return errors.New("Cannot set blank value for RootFS provider")
	}

	*p = append(*p, value)
	return nil
}

type argList []string

func (a *argList) String() string {
	return fmt.Sprintf("%v", *a)
}

func (a *argList) Set(value string) error {
	*a = strings.Split(value, ",")
	return nil
}

const (
	dropsondeOrigin = "rep"

	bbsPingTimeout = 5 * time.Minute
)

func main() {
	cf_debug_server.AddFlags(flag.CommandLine)
	cf_lager.AddFlags(flag.CommandLine)

	stackMap := stackPathMap{}
	supportedProviders := providers{}
	gardenHealthcheckEnv := argList{}
	gardenHealthcheckArgs := argList{}
	flag.Var(&stackMap, "preloadedRootFS", "List of preloaded RootFSes")
	flag.Var(&supportedProviders, "rootFSProvider", "List of RootFS providers")
	flag.Var(&gardenHealthcheckArgs, "gardenHealthcheckProcessArgs", "List of command line args to pass to the garden health check process")
	flag.Var(&gardenHealthcheckEnv, "gardenHealthcheckProcessEnv", "Environment variables to use when running the garden health check")
	flag.Parse()

	preloadedRootFSes := []string{}
	for k := range stackMap {
		preloadedRootFSes = append(preloadedRootFSes, k)
	}

	cf_http.Initialize(*communicationTimeout)

	clock := clock.NewClock()
	logger, reconfigurableSink := cf_lager.New(*sessionName)

	var executorConfiguration executorinit.Configuration
	var gardenHealthcheckRootFS string
	if len(preloadedRootFSes) == 0 {
		gardenHealthcheckRootFS = ""
	} else {
		gardenHealthcheckRootFS = stackMap[preloadedRootFSes[0]]
	}
	executorConfiguration = executorConfig(gardenHealthcheckRootFS, gardenHealthcheckArgs, gardenHealthcheckEnv)
	if !executorConfiguration.Validate(logger) {
		os.Exit(1)
	}

	initializeDropsonde(logger)

	if *cellID == "" {
		log.Fatalf("-cellID must be specified")
	}

	executorClient, executorMembers, err := executorinit.Initialize(logger, executorConfiguration, clock)
	if err != nil {
		log.Fatalf("Failed to initialize executor: %s", err.Error())
	}
	defer executorClient.Cleanup(logger)

	if err := validateBBSAddress(); err != nil {
		logger.Fatal("invalid-bbs-address", err)
	}

	serviceClient := initializeServiceClient(logger)

	evacuatable, evacuationReporter, evacuationNotifier := evacuation_context.New()

	// only one outstanding operation per container is necessary
	queue := operationq.NewSlidingQueue(1)

	evacuator := evacuation.NewEvacuator(
		logger,
		clock,
		executorClient,
		evacuationNotifier,
		*cellID,
		*evacuationTimeout,
		*evacuationPollingInterval,
	)

	bbsClient := initializeBBSClient(logger)
	httpServer, address := initializeServer(bbsClient, executorClient, evacuatable, evacuationReporter, logger, rep.StackPathMap(stackMap), supportedProviders)
	opGenerator := generator.New(*cellID, bbsClient, executorClient, evacuationReporter, uint64(evacuationTimeout.Seconds()))

	members := grouper.Members{
		{"http_server", httpServer},
		{"presence", initializeCellPresence(address, serviceClient, executorClient, logger, supportedProviders, preloadedRootFSes)},
		{"bulker", harmonizer.NewBulker(logger, *pollingInterval, *evacuationPollingInterval, evacuationNotifier, clock, opGenerator, queue)},
		{"event-consumer", harmonizer.NewEventConsumer(logger, opGenerator, queue)},
		{"evacuator", evacuator},
	}

	members = append(executorMembers, members...)

	if dbgAddr := cf_debug_server.DebugAddress(flag.CommandLine); dbgAddr != "" {
		members = append(grouper.Members{
			{"debug-server", cf_debug_server.Runner(dbgAddr, reconfigurableSink)},
		}, members...)
	}

	group := grouper.NewOrdered(os.Interrupt, members)

	monitor := ifrit.Invoke(sigmon.New(group))

	logger.Info("started", lager.Data{"cell-id": *cellID})

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")
}

func initializeDropsonde(logger lager.Logger) {
	dropsondeDestination := fmt.Sprint("localhost:", *dropsondePort)
	err := dropsonde.Initialize(dropsondeDestination, dropsondeOrigin)
	if err != nil {
		logger.Error("failed to initialize dropsonde: %v", err)
	}
}

func initializeCellPresence(address string, serviceClient bbs.ServiceClient, executorClient executor.Client, logger lager.Logger, rootFSProviders, preloadedRootFSes []string) ifrit.Runner {
	config := maintain.Config{
		CellID:            *cellID,
		RepAddress:        address,
		Zone:              *zone,
		RetryInterval:     *lockRetryInterval,
		RootFSProviders:   rootFSProviders,
		PreloadedRootFSes: preloadedRootFSes,
	}
	return maintain.New(logger, config, executorClient, serviceClient, *lockTTL, clock.NewClock())
}

func initializeServiceClient(logger lager.Logger) bbs.ServiceClient {
	consulClient, err := consuladapter.NewClientFromUrl(*consulCluster)
	if err != nil {
		logger.Fatal("new-client-failed", err)
	}

	return bbs.NewServiceClient(consulClient, clock.NewClock())
}

func initializeServer(
	bbsClient bbs.Client,
	executorClient executor.Client,
	evacuatable evacuation_context.Evacuatable,
	evacuationReporter evacuation_context.EvacuationReporter,
	logger lager.Logger,
	stackMap rep.StackPathMap,
	supportedProviders []string,
) (ifrit.Runner, string) {

	auctionCellRep := auction_cell_rep.New(*cellID, stackMap, supportedProviders, *zone, generateGuid, executorClient, evacuationReporter, logger)
	handlers := handlers.New(auctionCellRep, executorClient, evacuatable, logger)

	router, err := rata.NewRouter(rep.Routes, handlers)
	if err != nil {
		logger.Fatal("failed-to-construct-router", err)
	}

	addressParts := strings.Split(*listenAddr, ":")
	address := fmt.Sprintf("http://%s:%s", addressParts[0], addressParts[1])

	return http_server.New(*listenAddr, router), address
}

func validateBBSAddress() error {
	if *bbsAddress == "" {
		return errors.New("bbsAddress is required")
	}
	return nil
}

func generateGuid() (string, error) {
	guid, err := uuid.NewV4()
	if err != nil {
		return "", err
	}
	return guid.String(), nil
}

func initializeBBSClient(logger lager.Logger) bbs.Client {
	bbsURL, err := url.Parse(*bbsAddress)
	if err != nil {
		logger.Fatal("Invalid BBS URL", err)
	}

	if bbsURL.Scheme != "https" {
		return bbs.NewClient(*bbsAddress)
	}

	bbsClient, err := bbs.NewSecureClient(*bbsAddress, *bbsCACert, *bbsClientCert, *bbsClientKey, *bbsClientSessionCacheSize, *bbsMaxIdleConnsPerHost)
	if err != nil {
		logger.Fatal("Failed to configure secure BBS client", err)
	}
	return bbsClient
}
