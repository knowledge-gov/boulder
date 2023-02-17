package notmain

import (
	"flag"
	"os"
	"strings"

	"github.com/honeycombio/beeline-go"

	capb "github.com/letsencrypt/boulder/ca/proto"
	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/db"
	"github.com/letsencrypt/boulder/features"
	bgrpc "github.com/letsencrypt/boulder/grpc"
	ocsp_updater "github.com/letsencrypt/boulder/ocsp/updater"
	"github.com/letsencrypt/boulder/sa"
	"github.com/letsencrypt/validator/v10"
)

const cmdName = "ocsp-updater"

type Config struct {
	OCSPUpdater struct {
		cmd.ServiceConfig
		DB         cmd.DBConfig `validate:"required"`
		ReadOnlyDB cmd.DBConfig `validate:"required"`

		// Issuers is a map from filenames to short issuer IDs.
		// Each filename must contain an issuer certificate. The short issuer
		// IDs are arbitrarily assigned and must be consistent across OCSP
		// components. For production we'll use the number part of the CN, i.e.
		// E1 -> 1, R3 -> 3, etc.
		Issuers map[string]int `validate:"isdefault"`

		// OldOCSPWindow controls how frequently ocsp-updater signs a batch
		// of responses.
		OldOCSPWindow cmd.ConfigDuration `validate:"required"`
		// OldOCSPBatchSize controls the maximum number of responses
		// ocsp-updater will sign every OldOCSPWindow.
		OldOCSPBatchSize int `validate:"required"`

		// The worst-case freshness of a response during normal operations.
		// This is related to to ExpectedFreshness in ocsp-responder's config,
		// and both are related to the mandated refresh times in the BRs and
		// root programs (minus a safety margin).
		OCSPMinTimeToExpiry cmd.ConfigDuration `validate:"required"`

		// ParallelGenerateOCSPRequests determines how many requests to the CA
		// may be inflight at once.
		ParallelGenerateOCSPRequests int `validate:"required"`

		// TODO(#5933): Replace this with a unifed RetryBackoffConfig
		SignFailureBackoffFactor float64            `validate:"required"`
		SignFailureBackoffMax    cmd.ConfigDuration `validate:"required"`

		// SerialSuffixShards is a whitespace-separated list of single hex
		// digits. When searching for work to do, ocsp-updater will query
		// for only those certificates end in one of the specified hex digits.
		SerialSuffixShards string `validate:"suffixshards"`

		OCSPGeneratorService *cmd.GRPCClientConfig `validate:"required"`

		Features map[string]bool
	} `validate:"required"`

	Syslog  cmd.SyslogConfig
	Beeline cmd.BeelineConfig
}

func main() {
	configFile := flag.String("config", "", "File path to the configuration file for this service")
	validate := flag.Bool("validate", false, "Validate the config file and exit")
	flag.Parse()
	if *configFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	if *validate {
		err := cmd.ReadAndValidateConfigFile(cmdName, *configFile)
		cmd.FailOnError(err, "Failed to validate config file")
	}

	var c Config
	err := cmd.ReadConfigFile(*configFile, &c)
	cmd.FailOnError(err, "Reading JSON config file into config structure")

	conf := c.OCSPUpdater
	err = features.Set(conf.Features)
	cmd.FailOnError(err, "Failed to set feature flags")

	bc, err := c.Beeline.Load()
	cmd.FailOnError(err, "Failed to load Beeline config")
	beeline.Init(bc)
	defer beeline.Close()

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

// SuffixShardsVal implements validator.Func to validate the SerialSuffixShards
// field of the Config struct.
func SuffixShardsVal(fl validator.FieldLevel) bool {
	err := ocsp_updater.SerialSuffixShardsValid(strings.Fields(fl.Field().String()))
	return (err == nil)
}

func init() {
	cmd.RegisterCommand(cmdName, main)
	cmd.RegisterConfig(cmdName, &cmd.ConfigValidator{
		Config: &Config{},
		Validators: map[string]validator.Func{
			"suffixshards": SuffixShardsVal,
		},
	})
}
