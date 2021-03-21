package nodehealth

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/heptiolabs/healthcheck"
)

//Configuration struct
type configStruct struct {
	mu                         sync.Mutex
	pollingRate                time.Duration
	healthcheckRPC             string
	openethereumHealthcheckRPC string
	primaryHealthcheckRPC      string
}

type Log struct {
	Err    error
	Sev    string
	Var    string
	Comp   string
	Debug  bool
	Config bool

	ValStr    string
	ValBigInt big.Int
}

type inboxReaderState struct {
	getNextBlockToRead big.Int
	currentHeight      big.Int
	arbCorePosition    big.Int
	caughtUpTarget     big.Int
}

type healthState struct {
	mu          sync.Mutex
	inboxReader inboxReaderState
}

type aSyncDataStruct struct {
	//Healthchecks to allow the status to be shared between handlers
	checkOpenethereum healthcheck.Check
	checkPrimary      healthcheck.Check
	inboxReaderStatus healthcheck.Check
}

func Logger(config *configStruct, state *healthState, logMsgChan <-chan Log) {
	for {
		logMessage := <-logMsgChan
		fmt.Println(logMessage)
		if logMessage.Config == true {
			if logMessage.Var == "openethereumHealthcheckRPC" {
				config.mu.Lock()
				config.openethereumHealthcheckRPC = logMessage.ValStr
				config.mu.Unlock()
			}
			if logMessage.Var == "primaryHealthcheckRPC" {
				config.mu.Lock()
				config.primaryHealthcheckRPC = logMessage.ValStr
				config.mu.Unlock()
			}
		} else {
			if logMessage.Comp == "caughtUpTarget" {
				if logMessage.Var == "getNextBlockToRead" {
					state.mu.Lock()
					state.inboxReader.getNextBlockToRead = logMessage.ValBigInt
					state.mu.Unlock()
				}
				if logMessage.Var == "currentHeight" {
					state.mu.Lock()
					state.inboxReader.currentHeight = logMessage.ValBigInt
					state.mu.Unlock()
				}
				if logMessage.Var == "arbCorePosition" {
					state.mu.Lock()
					state.inboxReader.arbCorePosition = logMessage.ValBigInt
					state.mu.Unlock()
				}
				if logMessage.Var == "caughtUpTarget" {
					state.mu.Lock()
					state.inboxReader.caughtUpTarget = logMessage.ValBigInt
					state.mu.Unlock()
				}
			}
		}
	}
}

// Default configuration values for the healthcheck server
func (config *configStruct) loadConfig() {
	config.healthcheckRPC = "0.0.0.0:8080"
	config.pollingRate = 10 * time.Second
	config.openethereumHealthcheckRPC = ""
	config.primaryHealthcheckRPC = ""
}

//Perform all upstream checks at a set time interval in an asynchronous manner
func aSyncUpstream(aSyncData *aSyncDataStruct, state *healthState, config *configStruct) {
	aSyncData.checkOpenethereum = healthcheck.Async(func() error {
		res, err := http.Get("http://" + config.openethereumHealthcheckRPC + "/live")
		if err != nil {
			return err
		} else {
			if res.StatusCode != 200 {
				//The server is returning an unexpected status code
				return errors.New("OpenEthereum not ready")
			}
		}
		return nil
	}, config.pollingRate)

	aSyncData.checkPrimary = healthcheck.Async(func() error {
		res, err := http.Get("http://" + config.primaryHealthcheckRPC + "/live")
		if err != nil {
			return err
		} else {
			if res.StatusCode != 200 {
				//The server is returning an unexpected status code
				return errors.New("Primary not ready")
			}
		}
		return nil
	}, config.pollingRate)

	aSyncData.inboxReaderStatus = healthcheck.Async(func() error {
		state.mu.Lock()
		blockDifference := big.NewInt(0).Sub(&state.inboxReader.caughtUpTarget, &state.inboxReader.currentHeight)
		tolerance := big.NewInt(2)
		if blockDifference.CmpAbs(tolerance) == 0 {
			state.mu.Unlock()
			return errors.New("InboxReader catching up")
		}
		state.mu.Unlock()
		return nil
	}, config.pollingRate)
}

//Define which healthchecks to use for the readiness API and expose the readiness API
func openEthereumReadinessChecks(health healthcheck.Handler, httpMux *http.ServeMux,
	aSyncData *aSyncDataStruct, config configStruct) {

	//Add healthchecks to the readiness check
	health.AddReadinessCheck(
		"openethereum-status",
		aSyncData.checkOpenethereum)

	health.AddReadinessCheck(
		"primary-status",
		aSyncData.checkPrimary)

	health.AddReadinessCheck(
		"inbox-reader-status",
		aSyncData.inboxReaderStatus)

	//Create an endpoint to serve the readiness check
	httpMux.HandleFunc("/ready", health.ReadyEndpoint)
}

//Define which healthchecks to use for the liveness API and expose the liveness API
func openEthereumLivenessChecks(health healthcheck.Handler, httpMux *http.ServeMux,
	aSyncData *aSyncDataStruct, config configStruct) {
	//Create an endpoint to serve the liveness check
	httpMux.HandleFunc("/live", health.LiveEndpoint)
}

//Create the HTTP server and start a watchdog to monitor its return codes
func healthcheckServer(httpMux *http.ServeMux, config configStruct) {
	http.ListenAndServe(config.healthcheckRPC, httpMux)
}

func waitConfig(config *configStruct) {
	config.mu.Lock()
	for (config.openethereumHealthcheckRPC == "") || (config.primaryHealthcheckRPC == "") {
		config.mu.Unlock()
		time.Sleep(1 * time.Second)
		config.mu.Lock()
	}
	config.mu.Unlock()
}

//Start the healthcheck for OpenEthereum
func startHealthCheck(config *configStruct, state *healthState) {
	//Allocate storage for the aSync calls
	aSyncData := aSyncDataStruct{}
	//Create the main healthcheck handler
	health := healthcheck.NewHandler()
	//Create an HTTP server mux to serve the endpoints
	httpMux := http.NewServeMux()

	waitConfig(config)

	//Schedule the async calls
	aSyncUpstream(&aSyncData, state, config)

	//Define which healthchecks to use for the liveness API and expose the liveness API
	openEthereumLivenessChecks(health, httpMux, &aSyncData, *config)

	//Define which healthchecks to use for the readiness API and expose the readiness API
	openEthereumReadinessChecks(health, httpMux, &aSyncData, *config)

	//Create the HTTP server and start a watchdog to monitor its return codes
	healthcheckServer(httpMux, *config)
}

func NodeHealthCheck(logMsgChan <-chan Log) {
	//Create the configuration struct
	config := configStruct{}
	state := healthState{}
	//Load the default configuration
	config.loadConfig()

	go Logger(&config, &state, logMsgChan)

	startHealthCheck(&config, &state)
}
