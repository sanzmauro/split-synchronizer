// Split Agent for across Split's SDKs
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/splitio/go-toolkit/v5/logging"
	"github.com/splitio/split-synchronizer/v4/splitio/common"
	"github.com/splitio/split-synchronizer/v4/splitio/producer"
	"github.com/splitio/split-synchronizer/v4/splitio/proxy"

	"github.com/splitio/split-synchronizer/v4/conf"
	"github.com/splitio/split-synchronizer/v4/splitio"

	_ "net/http/pprof"

	syncLog "github.com/splitio/split-synchronizer/v4/splitio/log"
)

type configMap map[string]interface{}
type flagInformation struct {
	configFile             *string
	writeDefaultConfigFile *string
	asProxy                *bool
	versionInfo            *bool
	cliParametersMap       configMap
}

func parseCLIFlags() *flagInformation {
	cliFlags := &flagInformation{
		configFile:             flag.String("config", "splitio.agent.conf.json", "a configuration file"),
		writeDefaultConfigFile: flag.String("write-default-config", "", "write a default configuration file"),
		asProxy:                flag.Bool("proxy", false, "run as split server proxy to improve sdk performance"),
		versionInfo:            flag.Bool("version", false, "Print the version"),
	}

	// dinamically configuration parameters
	cliParameters := conf.CliParametersToRegister()
	cliParametersMap := make(configMap, len(cliParameters))
	for _, param := range cliParameters {
		switch param.AttributeType {
		case "string":
			cliParametersMap[param.Command] = flag.String(param.Command, param.DefaultValue.(string), param.Description)
			break
		case "[]string":
			cliParametersMap[param.Command] = flag.String(param.Command, strings.Join(param.DefaultValue.([]string), ","), param.Description)
			break
		case "int":
			cliParametersMap[param.Command] = flag.Int(param.Command, param.DefaultValue.(int), param.Description)
			break
		case "int64":
			cliParametersMap[param.Command] = flag.Int64(param.Command, param.DefaultValue.(int64), param.Description)
			break
		case "bool":
			cliParametersMap[param.Command] = flag.Bool(param.Command, param.DefaultValue.(bool), param.Description)
			break
		}
	}

	cliFlags.cliParametersMap = cliParametersMap
	flag.Parse()
	return cliFlags
}

func loadConfiguration(configFile *string, cliParametersMap configMap) error {
	//load default values
	conf.Initialize()
	//overwrite default values from configuration file
	err := conf.LoadFromFile(*configFile)
	if err != nil {
		return err
	}
	//overwrite with cli values
	conf.LoadFromArgs(cliParametersMap)

	return conf.ValidConfigs()
}

func setupLogger() logging.LoggerInterface {
	var err error
	var commonWriter io.Writer
	var fullWriter io.Writer
	var verboseWriter = ioutil.Discard
	var debugWriter = ioutil.Discard
	var fileWriter = ioutil.Discard
	var stdoutWriter = ioutil.Discard
	var slackWriter = ioutil.Discard

	if len(conf.Data.Logger.File) > 3 {
		fileWriter, err = logging.NewFileRotate(&logging.FileRotateOptions{
			MaxBytes:    conf.Data.Logger.FileMaxSize,
			BackupCount: conf.Data.Logger.FileBackupCount,
			Path:        conf.Data.Logger.File,
		})
		if err != nil {
			fmt.Printf("Error opening log file: %s \n", err.Error())
			fileWriter = ioutil.Discard
		} else {
			fmt.Printf("Log file: %s \n", conf.Data.Logger.File)
		}
	}

	if conf.Data.Logger.StdoutOn {
		stdoutWriter = os.Stdout
	}

	_, err = url.ParseRequestURI(conf.Data.Logger.SlackWebhookURL)
	if err == nil {
		slackWriter = syncLog.NewSlackWriter(conf.Data.Logger.SlackWebhookURL, conf.Data.Logger.SlackChannel, 30*time.Second)
	}

	commonWriter = io.MultiWriter(stdoutWriter, fileWriter)
	fullWriter = io.MultiWriter(commonWriter, slackWriter)

	level := logging.LevelInfo
	if conf.Data.Logger.VerboseOn {
		verboseWriter = commonWriter
		level = logging.LevelVerbose
	}

	if conf.Data.Logger.DebugOn {
		debugWriter = commonWriter
		if !conf.Data.Logger.VerboseOn {
			level = logging.LevelDebug
		}
	}

	buffered := [5]bool{}
	buffered[logging.LevelError-logging.LevelError] = true
	buffered[logging.LevelWarning-logging.LevelError] = true
	buffered[logging.LevelInfo-logging.LevelError] = true
	buffered[logging.LevelDebug-logging.LevelError] = false
	buffered[logging.LevelVerbose-logging.LevelError] = false
	return syncLog.NewHistoricLoggerWrapper(logging.NewLogger(&logging.LoggerOptions{
		StandardLoggerFlags: log.Ldate | log.Ltime | log.Lshortfile,
		Prefix:              "SPLITIO-AGENT ",
		VerboseWriter:       verboseWriter,
		DebugWriter:         debugWriter,
		InfoWriter:          commonWriter,
		WarningWriter:       commonWriter,
		ErrorWriter:         fullWriter,
		LogLevel:            level,
		ExtraFramesToSkip:   1,
	}), buffered, 5)
}

func main() {

	// TODO(mredolatti): REMOVE THIS!
	runtime.SetCPUProfileRate(500)
	go http.ListenAndServe("0.0.0.0:9090", nil)

	//reading command line options
	cliFlags := parseCLIFlags()

	//print the version
	if *cliFlags.versionInfo {
		fmt.Printf("\nSplit Synchronizer - Version: %s (%s) \n", splitio.Version, splitio.CommitVersion)
		return
	}

	//Show initial banner
	fmt.Println(splitio.ASCILogo)
	fmt.Printf("\nSplit Synchronizer - Version: %s (%s) \n", splitio.Version, splitio.CommitVersion)

	//writing a default configuration file if it is required by user
	if *cliFlags.writeDefaultConfigFile != "" {
		conf.WriteDefaultConfigFile(*cliFlags.writeDefaultConfigFile)
		return
	}

	//Initialize modules
	err := loadConfiguration(cliFlags.configFile, cliFlags.cliParametersMap)
	if err != nil {
		fmt.Printf("\nSplit Synchronizer - Initialization error: %s\n", err)
		os.Exit(common.ExitInvalidConfiguration)
	}

	logger := setupLogger()
	if *cliFlags.asProxy {
		// log.PostStartedMessageToSlack() // TODO(mredolatti)
		err = proxy.Start(logger)
	} else {
		// log.PostStartedMessageToSlack() // TODO(mredolatti)
		err = producer.Start(logger)
	}

	if err == nil {
		return
	}

	var initError *common.InitializationError
	if errors.As(err, &initError) {
		logger.Error("Failed to initialize the split sync: ", initError)
		os.Exit(initError.ExitCode())
	}

	os.Exit(common.ExitUndefined)
}
