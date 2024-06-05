/*
 * MIT License
 *
 * Copyright (c) 2023 EASL and the vHive community
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

package driver

import (
	"bytes"
	"container/list"
	"context"
	"crypto/tls"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"golang.org/x/net/http2"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gocarina/gocsv"
	log "github.com/sirupsen/logrus"
	"github.com/vhive-serverless/loader/pkg/common"
	"github.com/vhive-serverless/loader/pkg/config"
	"github.com/vhive-serverless/loader/pkg/generator"
	mc "github.com/vhive-serverless/loader/pkg/metric"
	"github.com/vhive-serverless/loader/pkg/trace"
)

type DriverConfiguration struct {
	LoaderConfiguration *config.LoaderConfiguration
	IATDistribution     common.IatDistribution
	ShiftIAT            bool // shift the invocations inside minute
	TraceGranularity    common.TraceGranularity
	TraceDuration       int // in minutes

	YAMLPath string
	TestMode bool

	Functions []*common.Function
}

type Driver struct {
	Configuration          *DriverConfiguration
	SpecificationGenerator *generator.SpecificationGenerator
	HTTPClient             *http.Client
	AsyncRecords           *common.LockFreeQueue[*mc.ExecutionRecord]
}

func NewDriver(driverConfig *DriverConfiguration) *Driver {
	return &Driver{
		Configuration:          driverConfig,
		SpecificationGenerator: generator.NewSpecificationGenerator(driverConfig.LoaderConfiguration.Seed),
		AsyncRecords:           common.NewLockFreeQueue[*mc.ExecutionRecord](),
	}
}

func (c *DriverConfiguration) WithWarmup() bool {
	if c.LoaderConfiguration.WarmupDuration > 0 {
		return true
	} else {
		return false
	}
}

// ///////////////////////////////////////
// HELPER METHODS
// ///////////////////////////////////////
func (d *Driver) outputFilename(name string) string {
	return fmt.Sprintf("%s_%s_%d.csv", d.Configuration.LoaderConfiguration.OutputPathPrefix, name, d.Configuration.TraceDuration)
}

func (d *Driver) runCSVWriter(records chan interface{}, filename string, writerDone *sync.WaitGroup) {
	log.Debugf("Starting writer for %s", filename)

	file, err := os.Create(filename)
	common.Check(err)
	defer file.Close()

	writer := gocsv.NewSafeCSVWriter(csv.NewWriter(file))
	if err := gocsv.MarshalChan(records, writer); err != nil {
		log.Fatal(err)
	}

	writerDone.Done()
}

func DAGCreation(functions []*common.Function) *list.List {
	linkedList := list.New()
	// Assigning nodes one after another
	for _, function := range functions {
		linkedList.PushBack(function)
	}
	return linkedList
}

func (d *Driver) GetHTTPClient() *http.Client {
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{
			Timeout: time.Duration(d.Configuration.LoaderConfiguration.GRPCFunctionTimeoutSeconds) * time.Second,
			// HTTP/2
			Transport: &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
					return net.Dial(network, addr)
				},
			},
			// HTTP/1.1
			/*Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: 10 * time.Second,
				}).DialContext,
				DisableCompression:  true,
				IdleConnTimeout:     60 * time.Second,
				MaxIdleConns:        3000,
				MaxIdleConnsPerHost: 3000,
			},*/
		}
	}

	return d.HTTPClient
}

/////////////////////////////////////////
// METRICS SCRAPPERS
/////////////////////////////////////////

func (d *Driver) CreateMetricsScrapper(interval time.Duration,
	signalReady *sync.WaitGroup, finishCh chan int, allRecordsWritten *sync.WaitGroup) func() {
	timer := time.NewTicker(interval)

	return func() {
		signalReady.Done()
		knStatRecords := make(chan interface{}, 100)
		scaleRecords := make(chan interface{}, 100)
		writerDone := sync.WaitGroup{}

		clusterUsageFile, err := os.Create(d.outputFilename("cluster_usage"))
		common.Check(err)
		defer clusterUsageFile.Close()

		writerDone.Add(1)
		go d.runCSVWriter(knStatRecords, d.outputFilename("kn_stats"), &writerDone)

		writerDone.Add(1)
		go d.runCSVWriter(scaleRecords, d.outputFilename("deployment_scale"), &writerDone)

		for {
			select {
			case <-timer.C:
				recCluster := mc.ScrapeClusterUsage()
				recCluster.Timestamp = time.Now().UnixMicro()

				byteArr, err := json.Marshal(recCluster)
				common.Check(err)

				_, err = clusterUsageFile.Write(byteArr)
				common.Check(err)

				_, err = clusterUsageFile.WriteString("\n")
				common.Check(err)

				recScale := mc.ScrapeDeploymentScales()
				timestamp := time.Now().UnixMicro()
				for _, rec := range recScale {
					rec.Timestamp = timestamp
					scaleRecords <- rec
				}

				recKnative := mc.ScrapeKnStats()
				recKnative.Timestamp = time.Now().UnixMicro()
				knStatRecords <- recKnative
			case <-finishCh:
				close(knStatRecords)
				close(scaleRecords)

				writerDone.Wait()
				allRecordsWritten.Done()

				return
			}
		}
	}
}

/////////////////////////////////////////
// DRIVER LOGIC
/////////////////////////////////////////

type InvocationMetadata struct {
	RootFunction *list.List
	Phase        common.ExperimentPhase

	MinuteIndex     int
	InvocationIndex int

	SuccessCount        *int64
	FailedCount         *int64
	FailedCountByMinute []int64

	RecordOutputChannel   chan interface{}
	AnnounceDoneWG        *sync.WaitGroup
	AnnounceDoneExe       *sync.WaitGroup
	ReadOpenWhiskMetadata *sync.Mutex
}

func composeInvocationID(timeGranularity common.TraceGranularity, minuteIndex int, invocationIndex int) string {
	var timePrefix string

	switch timeGranularity {
	case common.MinuteGranularity:
		timePrefix = "min"
	case common.SecondGranularity:
		timePrefix = "sec"
	default:
		log.Fatal("Invalid trace granularity parameter.")
	}

	return fmt.Sprintf("%s%d.inv%d", timePrefix, minuteIndex, invocationIndex)
}

func (d *Driver) invokeFunction(metadata *InvocationMetadata, iatIndex int) {
	defer metadata.AnnounceDoneWG.Done()

	var success bool
	var record *mc.ExecutionRecord
	var runtimeSpecifications *common.RuntimeSpecification

	node := metadata.RootFunction.Front()
	for node != nil {
		function := node.Value.(*common.Function)
		runtimeSpecifications = &function.Specification.RuntimeSpecification[iatIndex]

		switch d.Configuration.LoaderConfiguration.Platform {
		case "Knative", "Knative-RPS":
			success, record = InvokeGRPC(
				function,
				runtimeSpecifications,
				d.Configuration.LoaderConfiguration,
			)
		case "OpenWhisk", "OpenWhisk-RPS":
			success, record = InvokeOpenWhisk(
				function,
				runtimeSpecifications,
				metadata.AnnounceDoneExe,
				metadata.ReadOpenWhiskMetadata,
			)
		case "AWSLambda", "AWSLambda-RPS":
			success, record = InvokeAWSLambda(
				function,
				runtimeSpecifications,
				metadata.AnnounceDoneExe,
			)
		case "Dirigent", "Dirigent-RPS":
			success, record = InvokeDirigent(
				function,
				runtimeSpecifications,
				d.GetHTTPClient(),
				d.Configuration.LoaderConfiguration,
			)
		default:
			log.Fatal("Unsupported platform.")
		}
		record.Phase = int(metadata.Phase)
		record.InvocationID = composeInvocationID(d.Configuration.TraceGranularity, metadata.MinuteIndex, metadata.InvocationIndex)

		if !d.Configuration.LoaderConfiguration.AsyncMode || record.AsyncResponseGUID == "" {
			metadata.RecordOutputChannel <- record
		} else {
			record.TimeToSubmitMs = record.ResponseTime
			d.AsyncRecords.Enqueue(record)
		}

		if !success {
			log.Debugf("Invocation failed at minute: %d for %s", metadata.MinuteIndex, function.Name)

			atomic.AddInt64(metadata.FailedCount, 1)
			atomic.AddInt64(&metadata.FailedCountByMinute[metadata.MinuteIndex], 1)

			break
		}

		node = node.Next()
		atomic.AddInt64(metadata.SuccessCount, 1)
	}
}

func (d *Driver) functionsDriver(list *list.List, announceFunctionDone *sync.WaitGroup,
	addInvocationsToGroup *sync.WaitGroup, readOpenWhiskMetadata *sync.Mutex, totalSuccessful *int64,
	totalFailed *int64, totalIssued *int64, recordOutputChannel chan interface{}) {

	function := list.Front().Value.(*common.Function)
	numberOfInvocations := 0
	for i := 0; i < len(function.Specification.PerMinuteCount); i++ {
		numberOfInvocations += function.Specification.PerMinuteCount[i]
	}
	addInvocationsToGroup.Add(numberOfInvocations)

	totalTraceDuration := d.Configuration.TraceDuration
	minuteIndex, invocationIndex := 0, 0

	IAT := function.Specification.IAT

	var successfulInvocations int64
	var failedInvocations int64
	var failedInvocationByMinute = make([]int64, totalTraceDuration)
	var numberOfIssuedInvocations int64
	var currentPhase = common.ExecutionPhase

	waitForInvocations := sync.WaitGroup{}

	currentMinute, currentSum := 0, 0

	if d.Configuration.WithWarmup() {
		currentPhase = common.WarmupPhase
		// skip the first minute because of profiling
		minuteIndex = 1
		currentMinute = 1

		log.Infof("Warmup phase has started.")
	}

	startOfMinute := time.Now()
	var previousIATSum int64

	for {
		if minuteIndex != currentMinute {
			// postpone summation of invocation count for the beginning of each minute
			currentSum += function.Specification.PerMinuteCount[currentMinute]
			currentMinute = minuteIndex
		}

		iatIndex := currentSum + invocationIndex

		if minuteIndex >= totalTraceDuration || iatIndex >= len(IAT) {
			// Check whether the end of trace has been reached
			break
		} else if function.Specification.PerMinuteCount[minuteIndex] == 0 {
			// Sleep for a minute if there are no invocations
			if d.proceedToNextMinute(function, &minuteIndex, &invocationIndex,
				&startOfMinute, true, &currentPhase, failedInvocationByMinute, &previousIATSum) {
				break
			}

			switch d.Configuration.TraceGranularity {
			case common.MinuteGranularity:
				time.Sleep(time.Minute)
			case common.SecondGranularity:
				time.Sleep(time.Second)
			default:
				log.Fatal("Unsupported trace granularity.")
			}

			continue
		}

		iat := time.Duration(IAT[iatIndex]) * time.Microsecond

		currentTime := time.Now()
		schedulingDelay := currentTime.Sub(startOfMinute).Microseconds() - previousIATSum
		sleepFor := iat.Microseconds() - schedulingDelay
		time.Sleep(time.Duration(sleepFor) * time.Microsecond)

		previousIATSum += iat.Microseconds()

		if !d.Configuration.TestMode {
			waitForInvocations.Add(1)

			go d.invokeFunction(&InvocationMetadata{
				RootFunction:          list,
				Phase:                 currentPhase,
				MinuteIndex:           minuteIndex,
				InvocationIndex:       invocationIndex,
				SuccessCount:          &successfulInvocations,
				FailedCount:           &failedInvocations,
				FailedCountByMinute:   failedInvocationByMinute,
				RecordOutputChannel:   recordOutputChannel,
				AnnounceDoneWG:        &waitForInvocations,
				AnnounceDoneExe:       addInvocationsToGroup,
				ReadOpenWhiskMetadata: readOpenWhiskMetadata,
			}, iatIndex)
		} else {
			// To be used from within the Golang testing framework
			log.Debugf("Test mode invocation fired.\n")

			recordOutputChannel <- &mc.ExecutionRecordBase{
				Phase:        int(currentPhase),
				InvocationID: composeInvocationID(d.Configuration.TraceGranularity, minuteIndex, invocationIndex),
				StartTime:    time.Now().UnixNano(),
			}

			successfulInvocations++
		}

		numberOfIssuedInvocations++

		if function.Specification.PerMinuteCount[minuteIndex] == invocationIndex || hasMinuteExpired(startOfMinute) {
			readyToBreak := d.proceedToNextMinute(function, &minuteIndex, &invocationIndex, &startOfMinute,
				false, &currentPhase, failedInvocationByMinute, &previousIATSum)

			if readyToBreak {
				break
			}
		} else {
			invocationIndex++
		}
	}

	waitForInvocations.Wait()

	log.Debugf("All the invocations for function %s have been completed.\n", function.Name)
	announceFunctionDone.Done()

	atomic.AddInt64(totalSuccessful, successfulInvocations)
	atomic.AddInt64(totalFailed, failedInvocations)
	atomic.AddInt64(totalIssued, numberOfIssuedInvocations)
}

func (d *Driver) proceedToNextMinute(function *common.Function, minuteIndex *int, invocationIndex *int, startOfMinute *time.Time,
	skipMinute bool, currentPhase *common.ExperimentPhase, failedInvocationByMinute []int64, previousIATSum *int64) bool {

	// TODO: fault check disabled for now; refactor the commented code below
	/*if d.Configuration.TraceGranularity == common.MinuteGranularity && !strings.HasSuffix(d.Configuration.LoaderConfiguration.Platform, "-RPS") {
		if !isRequestTargetAchieved(function.Specification.PerMinuteCount[*minuteIndex], *invocationIndex, common.RequestedVsIssued) {
			// Not fatal because we want to keep the measurements to be written to the output file
			log.Warnf("Relative difference between requested and issued number of invocations is greater than %.2f%%. Terminating function driver for %s!\n", common.RequestedVsIssuedTerminateThreshold*100, function.Name)

			return true
		}

		for i := 0; i <= *minuteIndex; i++ {
			notFailedCount := function.Specification.PerMinuteCount[i] - int(atomic.LoadInt64(&failedInvocationByMinute[i]))
			if !isRequestTargetAchieved(function.Specification.PerMinuteCount[i], notFailedCount, common.IssuedVsFailed) {
				// Not fatal because we want to keep the measurements to be written to the output file
				log.Warnf("Percentage of failed request is greater than %.2f%%. Terminating function driver for %s!\n", common.FailedTerminateThreshold*100, function.Name)

				return true
			}
		}
	}*/

	*minuteIndex++
	*invocationIndex = 0
	*previousIATSum = 0

	if d.Configuration.WithWarmup() && *minuteIndex == (d.Configuration.LoaderConfiguration.WarmupDuration+1) {
		*currentPhase = common.ExecutionPhase
		log.Infof("Warmup phase has finished. Starting the execution phase.")
	}

	if !skipMinute {
		*startOfMinute = time.Now()
	} else {
		switch d.Configuration.TraceGranularity {
		case common.MinuteGranularity:
			*startOfMinute = time.Now().Add(time.Minute)
		case common.SecondGranularity:
			*startOfMinute = time.Now().Add(time.Second)
		default:
			log.Fatal("Unsupported trace granularity.")
		}
	}

	return false
}

func isRequestTargetAchieved(ideal int, real int, assertType common.RuntimeAssertType) bool {
	if ideal == 0 {
		return true
	}

	ratio := float64(ideal-real) / float64(ideal)

	var warnBound float64
	var terminationBound float64
	var warnMessage string

	switch assertType {
	case common.RequestedVsIssued:
		warnBound = common.RequestedVsIssuedWarnThreshold
		terminationBound = common.RequestedVsIssuedTerminateThreshold
		warnMessage = fmt.Sprintf("Relative difference between requested and issued number of invocations has reached %.2f.", ratio)
	case common.IssuedVsFailed:
		warnBound = common.FailedWarnThreshold
		terminationBound = common.FailedTerminateThreshold
		warnMessage = fmt.Sprintf("Percentage of failed invocations within a minute has reached %.2f.", ratio)
	default:
		log.Fatal("Invalid type of assertion at runtime.")
	}

	if ratio < 0 || ratio > 1 {
		log.Fatalf("Invalid arguments provided to runtime assertion.\n")
	} else if ratio >= terminationBound {
		return false
	}

	if ratio >= warnBound && ratio < terminationBound {
		log.Warn(warnMessage)
	}

	return true
}

func hasMinuteExpired(t1 time.Time) bool {
	return time.Since(t1) > time.Minute
}

func (d *Driver) globalTimekeeper(totalTraceDuration int, signalReady *sync.WaitGroup) {
	ticker := time.NewTicker(time.Minute)
	globalTimeCounter := 0

	signalReady.Done()

	for {
		<-ticker.C

		log.Debugf("End of minute %d\n", globalTimeCounter)
		globalTimeCounter++
		if globalTimeCounter >= totalTraceDuration {
			break
		}

		log.Debugf("Start of minute %d\n", globalTimeCounter)
	}

	ticker.Stop()
}

func (d *Driver) createGlobalMetricsCollector(filename string, collector chan interface{},
	signalReady *sync.WaitGroup, signalEverythingWritten *sync.WaitGroup, totalIssuedChannel chan int64) {

	// NOTE: totalNumberOfInvocations is initialized to MaxInt64 not to allow collector to complete before
	// the end signal is received on totalIssuedChannel, which deliver the total number of issued invocations.
	// This number is known once all the individual function drivers finish issuing invocations and
	// when all the invocations return
	var totalNumberOfInvocations int64 = math.MaxInt64
	var currentlyWritten int64

	file, err := os.Create(filename)
	common.Check(err)
	defer file.Close()
	signalReady.Done()

	records := make(chan interface{}, 100)
	writerDone := sync.WaitGroup{}
	writerDone.Add(1)
	go d.runCSVWriter(records, filename, &writerDone)

	for {
		select {
		case record := <-collector:
			records <- record

			currentlyWritten++
		case record := <-totalIssuedChannel:
			totalNumberOfInvocations = record
		}

		if currentlyWritten == totalNumberOfInvocations {
			close(records)
			writerDone.Wait()
			(*signalEverythingWritten).Done()

			return
		}
	}
}

func (d *Driver) startBackgroundProcesses(allRecordsWritten *sync.WaitGroup) (*sync.WaitGroup, chan interface{}, chan int64, chan int) {
	auxiliaryProcessBarrier := &sync.WaitGroup{}

	finishCh := make(chan int, 1)

	if d.Configuration.LoaderConfiguration.EnableMetricsScrapping {
		auxiliaryProcessBarrier.Add(1)

		allRecordsWritten.Add(1)
		metricsScrapper := d.CreateMetricsScrapper(time.Second*time.Duration(d.Configuration.LoaderConfiguration.MetricScrapingPeriodSeconds), auxiliaryProcessBarrier, finishCh, allRecordsWritten)
		go metricsScrapper()
	}

	auxiliaryProcessBarrier.Add(2)

	globalMetricsCollector := make(chan interface{})
	totalIssuedChannel := make(chan int64)
	go d.createGlobalMetricsCollector(d.outputFilename("duration"), globalMetricsCollector, auxiliaryProcessBarrier, allRecordsWritten, totalIssuedChannel)

	traceDurationInMinutes := d.Configuration.TraceDuration
	go d.globalTimekeeper(traceDurationInMinutes, auxiliaryProcessBarrier)

	return auxiliaryProcessBarrier, globalMetricsCollector, totalIssuedChannel, finishCh
}

func (d *Driver) internalRun(skipIATGeneration bool, readIATFromFile bool) {
	var successfulInvocations int64
	var failedInvocations int64
	var invocationsIssued int64
	var functionsPerDAG int64
	readOpenWhiskMetadata := sync.Mutex{}
	allFunctionsInvoked := sync.WaitGroup{}
	allIndividualDriversCompleted := sync.WaitGroup{}
	allRecordsWritten := sync.WaitGroup{}
	allRecordsWritten.Add(1)

	backgroundProcessesInitializationBarrier, globalMetricsCollector, totalIssuedChannel, scraperFinishCh := d.startBackgroundProcesses(&allRecordsWritten)

	if !skipIATGeneration {
		log.Info("Generating IAT and runtime specifications for all the functions")
		for i, function := range d.Configuration.Functions {
			// Equalising all the InvocationStats to the first function
			if d.Configuration.LoaderConfiguration.DAGMode {
				function.InvocationStats.Invocations = d.Configuration.Functions[0].InvocationStats.Invocations
			}
			spec := d.SpecificationGenerator.GenerateInvocationData(
				function,
				d.Configuration.IATDistribution,
				d.Configuration.ShiftIAT,
				d.Configuration.TraceGranularity,
			)

			d.Configuration.Functions[i].Specification = spec
		}
	}

	backgroundProcessesInitializationBarrier.Wait()

	if readIATFromFile {
		for i := range d.Configuration.Functions {
			var spec common.FunctionSpecification

			iatFile, _ := os.ReadFile("iat" + strconv.Itoa(i) + ".json")
			err := json.Unmarshal(iatFile, &spec)
			if err != nil {
				log.Fatalf("Failed tu unmarshal iat file: %s", err)
			}

			d.Configuration.Functions[i].Specification = &spec
		}
	}

	if d.Configuration.LoaderConfiguration.DAGMode {
		log.Infof("Starting DAG invocation driver\n")
		functionLinkedList := DAGCreation(d.Configuration.Functions)
		functionsPerDAG = int64(len(d.Configuration.Functions))
		allIndividualDriversCompleted.Add(1)
		go d.functionsDriver(
			functionLinkedList,
			&allIndividualDriversCompleted,
			&allFunctionsInvoked,
			&readOpenWhiskMetadata,
			&successfulInvocations,
			&failedInvocations,
			&invocationsIssued,
			globalMetricsCollector,
		)
	} else {
		log.Infof("Starting function invocation driver\n")
		functionsPerDAG = 1
		for _, function := range d.Configuration.Functions {
			allIndividualDriversCompleted.Add(1)
			linkedList := list.New()
			linkedList.PushBack(function)
			go d.functionsDriver(
				linkedList,
				&allIndividualDriversCompleted,
				&allFunctionsInvoked,
				&readOpenWhiskMetadata,
				&successfulInvocations,
				&failedInvocations,
				&invocationsIssued,
				globalMetricsCollector,
			)
		}
	}
	allIndividualDriversCompleted.Wait()
	if atomic.LoadInt64(&successfulInvocations)+atomic.LoadInt64(&failedInvocations) != 0 {
		log.Debugf("Waiting for all invocations record to be written.\n")

		if d.Configuration.LoaderConfiguration.AsyncMode {
			sleepFor := time.Duration(d.Configuration.LoaderConfiguration.AsyncWaitToCollectMin) * time.Minute

			log.Infof("Sleeping for %v...", sleepFor)
			time.Sleep(sleepFor)

			d.writeAsyncRecordsToLog(globalMetricsCollector)
		}

		totalIssuedChannel <- atomic.LoadInt64(&invocationsIssued) * functionsPerDAG
		scraperFinishCh <- 0 // Ask the scraper to finish metrics collection

		allRecordsWritten.Wait()
	}

	statSuccess := atomic.LoadInt64(&successfulInvocations)
	statFailed := atomic.LoadInt64(&failedInvocations)

	log.Infof("Trace has finished executing function invocation driver\n")
	log.Infof("Number of successful invocations: \t%d", statSuccess)
	log.Infof("Number of failed invocations: \t%d", statFailed)
	log.Infof("Total invocations: \t%d", statSuccess+statFailed)
	log.Infof("Failure rate: \t%.2f", float64(statFailed)/float64(statSuccess+statFailed))
}

func (d *Driver) writeAsyncRecordsToLog(logCh chan interface{}) {
	client := http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
			}).DialContext,
			DisableCompression:  true,
			IdleConnTimeout:     60 * time.Second,
			MaxIdleConns:        3000,
			MaxIdleConnsPerHost: 3000,
		},
	}

	const batchSize = 50
	currentBatch := 0
	totalBatches := int(math.Ceil(float64(d.AsyncRecords.Length()) / float64(batchSize)))

	log.Infof("Gathering functions responses...")
	for d.AsyncRecords.Length() > 0 {
		currentBatch++

		toProcess := batchSize
		if d.AsyncRecords.Length() < batchSize {
			toProcess = d.AsyncRecords.Length()
		}

		wg := sync.WaitGroup{}
		wg.Add(toProcess)

		for i := 0; i < toProcess; i++ {
			go func() {
				defer wg.Done()

				start := time.Now()

				record := d.AsyncRecords.Dequeue()
				response, e2e := d.getAsyncResponseData(
					&client,
					d.Configuration.LoaderConfiguration.AsyncResponseURL,
					record.AsyncResponseGUID,
				)

				if string(response) != "" {
					err := deserializeDirigentResponse(response, record)
					if err != nil {
						log.Errorf("Failed to deserialize Dirigent response - %v - %v", string(response), err)
					}
				} else {
					record.FunctionTimeout = true
					record.AsyncResponseGUID = ""
					log.Errorf("Failed to fetch response. The function has probably not yet completed.")
				}

				// loader send request + request e2e + loader get response
				timeToFetchResponse := time.Since(start).Microseconds()
				record.UserCodeExecutionMs = int64(e2e)
				record.TimeToGetResponseMs = timeToFetchResponse
				record.ResponseTime += int64(e2e)
				record.ResponseTime += timeToFetchResponse

				logCh <- record
			}()
		}

		wg.Wait()

		log.Infof("Processed %d/%d batches of async response gatherings", currentBatch, totalBatches)
	}

	log.Infof("Finished gathering async reponse answers")
}

func (d *Driver) getAsyncResponseData(client *http.Client, endpoint string, guid string) ([]byte, int) {
	req, err := http.NewRequest("GET", "http://"+endpoint, bytes.NewReader([]byte(guid)))
	if err != nil {
		log.Errorf("Failed to retrieve Dirigent response for %s - %v", guid, err)
		return []byte{}, 0
	}

	// TODO: set function name for load-balancing purpose
	//req.Header.Set("function", function.Name)

	resp, err := client.Do(req)
	if err != nil {
		log.Errorf("Failed to retrieve Dirigent response for %s - %v", guid, err)
		return []byte{}, 0
	}

	defer handleBodyClosing(resp)
	body, err := io.ReadAll(resp.Body)

	hdr := resp.Header.Get("Duration-Microseconds")
	e2e := 0

	if hdr != "" {
		e2e, err = strconv.Atoi(hdr)
		if err != nil {
			log.Errorf("Failed to parse end-to-end latency for %s - %v", guid, err)
		}
	}

	return body, e2e
}

func (d *Driver) RunExperiment(skipIATGeneration bool, readIATFromFIle bool) {
	if d.Configuration.WithWarmup() {
		trace.DoStaticTraceProfiling(d.Configuration.Functions)
	}

	trace.ApplyResourceLimits(d.Configuration.Functions, d.Configuration.LoaderConfiguration.CPULimit)

	switch d.Configuration.LoaderConfiguration.Platform {
	case "Knative", "Knative-RPS":
		DeployFunctions(d.Configuration.Functions,
			d.Configuration.YAMLPath,
			d.Configuration.LoaderConfiguration.IsPartiallyPanic,
			d.Configuration.LoaderConfiguration.EndpointPort,
			d.Configuration.LoaderConfiguration.AutoscalingMetric)
		go scheduleFailure(d.Configuration.LoaderConfiguration)
	case "OpenWhisk", "OpenWhisk-RPS":
		DeployFunctionsOpenWhisk(d.Configuration.Functions)
	case "AWSLambda", "AWSLambda-RPS":
		DeployFunctionsAWSLambda(d.Configuration.Functions)
	case "Dirigent", "Dirigent-RPS", "Dirigent-Dandelion", "Dirigent-Dandelion-RPS":
		DeployDirigent(d.Configuration.LoaderConfiguration.DirigentControlPlaneIP, d.Configuration.Functions)
		go scheduleFailure(d.Configuration.LoaderConfiguration)
	default:
		log.Fatal("Unsupported platform.")
	}

	// Generate load
	d.internalRun(skipIATGeneration, readIATFromFIle)

	// Clean up
	if d.Configuration.LoaderConfiguration.Platform == "Knative" {
		CleanKnative()
	} else if d.Configuration.LoaderConfiguration.Platform == "OpenWhisk" {
		CleanOpenWhisk(d.Configuration.Functions)
	} else if d.Configuration.LoaderConfiguration.Platform == "AWSLambda" {
		CleanAWSLambda(d.Configuration.Functions)
	}
}
