package notmain

import (
	"flag"
	"os"

	"github.com/honeycombio/beeline-go"
	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/db"
	"github.com/letsencrypt/boulder/features"
	bgrpc "github.com/letsencrypt/boulder/grpc"
	rocsp_config "github.com/letsencrypt/boulder/rocsp/config"
	"github.com/letsencrypt/boulder/sa"
	sapb "github.com/letsencrypt/boulder/sa/proto"
)

type Config struct {
	SA struct {
		cmd.ServiceConfig
		DB          cmd.DBConfig
		ReadOnlyDB  cmd.DBConfig
		IncidentsDB cmd.DBConfig
		Redis       *rocsp_config.RedisConfig
		Issuers     map[string]int

		Features map[string]bool

		// Max simultaneous SQL queries caused by a single RPC.
		ParallelismPerRPC int
	}

	Syslog  cmd.SyslogConfig
	Beeline cmd.BeelineConfig
}

func main() {
	grpcAddr := flag.String("addr", "", "gRPC listen address override")
	debugAddr := flag.String("debug-addr", "", "Debug server address override")
	configFile := flag.String("config", "", "File path to the configuration file for this service")
	flag.Parse()
	if *configFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	var c Config
	err := cmd.ReadConfigFile(*configFile, &c)
	cmd.FailOnError(err, "Reading JSON config file into config structure")

	err = features.Set(c.SA.Features)
	cmd.FailOnError(err, "Failed to set feature flags")

	if *grpcAddr != "" {
		c.SA.GRPC.Address = *grpcAddr
	}
	if *debugAddr != "" {
		c.SA.DebugAddr = *debugAddr
	}

	bc, err := c.Beeline.Load()
	cmd.FailOnError(err, "Failed to load Beeline config")
	beeline.Init(bc)
	defer beeline.Close()

	scope, logger := cmd.StatsAndLogging(c.Syslog, c.SA.DebugAddr)
	defer logger.AuditPanic()
	logger.Info(cmd.VersionString())

	dbMap, err := sa.InitWrappedDb(c.SA.DB, scope, logger)
	cmd.FailOnError(err, "While initializing dbMap")

	dbReadOnlyURL, err := c.SA.ReadOnlyDB.URL()
	cmd.FailOnError(err, "Couldn't load read-only DB URL")

	dbIncidentsURL, err := c.SA.IncidentsDB.URL()
	cmd.FailOnError(err, "Couldn't load incidents DB URL")

	var dbReadOnlyMap *db.WrappedMap
	if dbReadOnlyURL == "" {
		dbReadOnlyMap = dbMap
	} else {
		dbReadOnlyMap, err = sa.InitWrappedDb(c.SA.ReadOnlyDB, scope, logger)
		cmd.FailOnError(err, "While initializing dbReadOnlyMap")
	}

	var dbIncidentsMap *db.WrappedMap
	if dbIncidentsURL == "" {
		dbIncidentsMap = dbMap
	} else {
		dbIncidentsMap, err = sa.InitWrappedDb(c.SA.IncidentsDB, scope, logger)
		cmd.FailOnError(err, "While initializing dbIncidentsMap")
	}

	clk := cmd.Clock()

	shortIssuers, err := rocsp_config.LoadIssuers(c.SA.Issuers)
	cmd.FailOnError(err, "loading issuers")

	parallel := c.SA.ParallelismPerRPC
	if parallel < 1 {
		parallel = 1
	}
	sai, err := sa.NewSQLStorageAuthority(dbMap, dbReadOnlyMap, dbIncidentsMap, shortIssuers, clk, logger, scope, parallel)
	cmd.FailOnError(err, "Failed to create SA impl")

	tls, err := c.SA.TLS.Load()
	cmd.FailOnError(err, "TLS config")

	start, stop, err := bgrpc.NewServer(c.SA.GRPC).Add(
		&sapb.StorageAuthority_ServiceDesc, sai).Build(tls, scope, clk, bgrpc.NoCancelInterceptor)
	cmd.FailOnError(err, "Unable to setup SA gRPC server")

	go cmd.CatchSignals(logger, stop)
	cmd.FailOnError(start(), "SA gRPC service failed")
}

func init() {
	cmd.RegisterCommand("boulder-sa", main)
}
