package notmain

import (
	"flag"
	"os"
	"strings"

	capb "github.com/letsencrypt/boulder/ca/proto"
	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/config"
	"github.com/letsencrypt/boulder/db"
	"github.com/letsencrypt/boulder/features"
	bgrpc "github.com/letsencrypt/boulder/grpc"
	ocsp_updater "github.com/letsencrypt/boulder/ocsp/updater"
	"github.com/letsencrypt/boulder/sa"
)

type Config struct {
	OCSPUpdater struct {
		DebugAddr string

		// TLS client certificate, private key, and trusted root bundle.
		TLS        cmd.TLSConfig
		DB         cmd.DBConfig
		ReadOnlyDB cmd.DBConfig

		// OldOCSPWindow controls how frequently ocsp-updater signs a batch
		// of responses.
		OldOCSPWindow config.Duration
		// OldOCSPBatchSize controls the maximum number of responses
		// ocsp-updater will sign every OldOCSPWindow.
		OldOCSPBatchSize int

		// The worst-case freshness of a response during normal operations.
		// This is related to to ExpectedFreshness in ocsp-responder's config,
		// and both are related to the mandated refresh times in the BRs and
		// root programs (minus a safety margin).
		OCSPMinTimeToExpiry config.Duration

		// ParallelGenerateOCSPRequests determines how many requests to the CA
		// may be inflight at once.
		ParallelGenerateOCSPRequests int

		// TODO(#5933): Replace this with a unifed RetryBackoffConfig
		SignFailureBackoffFactor float64
		SignFailureBackoffMax    config.Duration

		// SerialSuffixShards is a whitespace-separated list of single hex
		// digits. When searching for work to do, ocsp-updater will query
		// for only those certificates end in one of the specified hex digits.
		SerialSuffixShards string

		OCSPGeneratorService *cmd.GRPCClientConfig

		Features map[string]bool
	}

	Syslog  cmd.SyslogConfig
	Beeline cmd.BeelineConfig
}

func main() {
	configFile := flag.String("config", "", "File path to the configuration file for this service")
	flag.Parse()
	if *configFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	var c Config
	err := cmd.ReadConfigFile(*configFile, &c)
	cmd.FailOnError(err, "Reading JSON config file into config structure")

	conf := c.OCSPUpdater
	err = features.Set(conf.Features)
	cmd.FailOnError(err, "Failed to set feature flags")

	scope, logger := cmd.StatsAndLogging(c.Syslog, conf.DebugAddr)
	defer logger.AuditPanic()
	logger.Info(cmd.VersionString())

	readWriteDb, err := sa.InitWrappedDb(conf.DB, scope, logger)
	cmd.FailOnError(err, "Failed to initialize database client")

	var readOnlyDb *db.WrappedMap
	readOnlyDbDSN, _ := conf.ReadOnlyDB.URL()
	if readOnlyDbDSN == "" {
		readOnlyDb = readWriteDb
	} else {
		readOnlyDb, err = sa.InitWrappedDb(conf.ReadOnlyDB, scope, logger)
		cmd.FailOnError(err, "Failed to initialize read-only database client")
	}

	clk := cmd.Clock()

	tlsConfig, err := c.OCSPUpdater.TLS.Load()
	cmd.FailOnError(err, "TLS config")

	caConn, err := bgrpc.ClientSetup(c.OCSPUpdater.OCSPGeneratorService, tlsConfig, scope, clk)
	cmd.FailOnError(err, "Failed to load credentials and create gRPC connection to CA")
	ogc := capb.NewOCSPGeneratorClient(caConn)

	var serialSuffixes []string
	if c.OCSPUpdater.SerialSuffixShards != "" {
		serialSuffixes = strings.Fields(c.OCSPUpdater.SerialSuffixShards)
	}

	updater, err := ocsp_updater.New(
		scope,
		clk,
		readWriteDb,
		readOnlyDb,
		serialSuffixes,
		ogc,
		conf.OldOCSPBatchSize,
		conf.OldOCSPWindow.Duration,
		conf.SignFailureBackoffMax.Duration,
		conf.SignFailureBackoffFactor,
		conf.OCSPMinTimeToExpiry.Duration,
		conf.ParallelGenerateOCSPRequests,
		logger,
	)
	cmd.FailOnError(err, "Failed to create updater")

	go cmd.CatchSignals(logger, nil)
	for {
		updater.Tick()
	}
}

func init() {
	cmd.RegisterCommand("ocsp-updater", main)
}
