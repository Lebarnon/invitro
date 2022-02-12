package generate

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	util "github.com/eth-easl/loader/pkg"
	fc "github.com/eth-easl/loader/pkg/function"
	mc "github.com/eth-easl/loader/pkg/metric"
	tc "github.com/eth-easl/loader/pkg/trace"
)

const pvalue = 0.05

/** Seed the math/rand package for it to be different on each run. */
func init() {
	rand.Seed(time.Now().UnixNano())
}

func GenerateInterarrivalTimesInMicro(invocationsPerMinute int, uniform bool) []float64 {
	rand.Seed(time.Now().UnixNano())
	oneSecondInMicro := 1000_000.0
	oneMinuteInMicro := 60*oneSecondInMicro - 1000

	rps := float64(invocationsPerMinute) / 60
	interArrivalTimes := []float64{}

	totoalDuration := 0.0
	for i := 0; i < invocationsPerMinute; i++ {
		var iat float64
		if uniform {
			iat = oneSecondInMicro / rps
		} else {
			iat = rand.ExpFloat64() / rps * oneSecondInMicro
		}
		//* Only guarantee microsecond-level accuracy.
		if iat < 1 {
			iat = 1
		}
		interArrivalTimes = append(interArrivalTimes, iat)
		totoalDuration += iat
	}

	if totoalDuration > oneMinuteInMicro {
		//* Normalise if it's longer than 1min.
		for i, iat := range interArrivalTimes {
			iat = iat / totoalDuration * oneMinuteInMicro
			if iat < 1 {
				iat = 1
			}
			interArrivalTimes[i] = iat
		}
	}

	// log.Info(stats.Sum(stats.LoadRawData(interArrivalTimes)))
	return interArrivalTimes
}

func GenerateLoads(
	phaseIdx int,
	phaseOffset int,
	withBlocking bool,
	rps int,
	functions []tc.Function,
	invocationsEachMinute [][]int,
	totalNumInvocationsEachMinute []int) int {

	ShuffleAllInvocationsInplace(&invocationsEachMinute)

	isFixedRate := true
	if rps < 1 {
		isFixedRate = false
	}

	start := time.Now()
	wg := sync.WaitGroup{}
	exporter := mc.NewExporter()
	idleDuration := time.Duration(0)
	totalDurationMinutes := len(totalNumInvocationsEachMinute)

	minute := 0

load_generation:
	for ; minute < int(totalDurationMinutes); minute++ {
		tick := 0
		var iats []float64

		rps = int(float64(totalNumInvocationsEachMinute[minute]) / 60)
		iats = GenerateInterarrivalTimesInMicro(
			totalNumInvocationsEachMinute[minute], isFixedRate)
		log.Infof("Minute[%d]\t RPS=%d", minute, rps)

		numFuncInvocaked := 0
		idleDuration = time.Duration(0)

		/** Set up timer to bound the one-minute invocation. */
		iterStart := time.Now()
		epsilon := time.Duration(0)
		timeout := time.After(time.Duration(60)*time.Second - epsilon)
		interval := time.Duration(iats[tick]) * time.Microsecond
		ticker := time.NewTicker(interval)
		done := make(chan bool)

		/** Launch a timer. */
		go func() {
			t := <-timeout
			log.Warn("(Slot finished)\t", t.Format(time.StampMilli), "\tMinute Nbr. ", minute)
			ticker.Stop()
			done <- true
		}()

		//* Bound the #invocations by `rps`.
		numInvocatonsThisMinute := util.MinOf(rps*60, totalNumInvocationsEachMinute[minute])
		var invocationCount int32

		for {
			select {
			case t := <-ticker.C:
				if tick >= numInvocatonsThisMinute {
					log.Info("Idle ticking at ", t.Format(time.StampMilli), "\tMinute Nbr. ", minute, " Itr. ", tick)
					idleDuration += interval
					continue
				}
				go func(m int, nxt int, phase int, rps int) {
					defer wg.Done()
					wg.Add(1)

					funcIndx := invocationsEachMinute[m][nxt]
					function := functions[funcIndx]
					//TODO: Make Dialling timeout customisable.
					diallingBound := 10 * time.Duration(function.RuntimeStats.Average) * time.Millisecond //* 2xruntime timeout for circumventing hanging.
					ctx, cancel := context.WithTimeout(context.Background(), diallingBound)
					defer cancel()

					hasInvoked, latencyRecord := fc.Invoke(ctx, function, GenerateExecutionSpecs)

					if hasInvoked {
						atomic.AddInt32(&invocationCount, 1)
						latencyRecord.Phase = phase
						latencyRecord.Rps = rps
						exporter.ReportLantency(latencyRecord)
					}
				}(minute, tick, phaseIdx, rps)
			case <-done:
				numFuncInvocaked += int(invocationCount)
				log.Info("Iteration spent: ", time.Since(iterStart), "\tMinute Nbr. ", minute)
				log.Info("Required #invocations=", totalNumInvocationsEachMinute[minute],
					" Fired #functions=", numFuncInvocaked, "\tMinute Nbr. ", minute)

				invocRecord := mc.MinuteInvocationRecord{
					MinuteIdx:        minute + phaseOffset,
					Phase:            phaseIdx,
					Rps:              rps,
					Duration:         time.Since(iterStart).Microseconds(),
					IdleDuration:     idleDuration.Microseconds(),
					NumFuncRequested: totalNumInvocationsEachMinute[minute],
					NumFuncInvoked:   numFuncInvocaked,
					NumFuncFailed:    numInvocatonsThisMinute - numFuncInvocaked,
				}
				exporter.ReportInvocation(invocRecord)

				//* Do not evaluate stationarity in the measurement phase (3).
				if phaseIdx != 3 && exporter.IsLatencyStationary(pvalue) {
					minute++
					break load_generation
				} else {
					goto next_minute
				}
			}
			//* Load the next inter-arrival time.
			tick++
			interval = time.Duration(iats[tick]) * time.Microsecond
			ticker = time.NewTicker(interval)
		}
	next_minute:
	}
	log.Info("\tFinished invoking all functions.\n\tStart waiting for all requests to return.")

	//* Hyperparameter for busy-wait
	delta := time.Duration(1) //TODO: Make this force wait customisable (currently it's the same duration as that of the traces).
	forceTimeoutDuration := time.Duration(totalDurationMinutes) * time.Minute / delta

	if !withBlocking {
		forceTimeoutDuration = time.Second * 1
	}

	if wgWaitWithTimeout(&wg, forceTimeoutDuration) {
		log.Warn("Time out waiting for ALL invocations to return.")
	} else {
		totalDuration := time.Since(start)
		log.Info("[No time out] Total invocation + waiting duration: ", totalDuration, "\tIdle ", idleDuration, "\n")
	}

	defer exporter.FinishAndSave(phaseIdx, minute)
	return phaseOffset + minute
}

/**
 * This function waits for the waitgroup for the specified max timeout.
 * Returns true if waiting timed out.
 */
func wgWaitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()
	select {
	case <-c:
		log.Info("Busy waiting finished at the end of all invocations.")
		return false
	case <-time.After(timeout):
		return true
	}
}

/**
 * This function has/uses side-effects, but for the sake of performance
 * keep it for now.
 */
func ShuffleAllInvocationsInplace(invocationsEachMinute *[][]int) {
	suffleOneMinute := func(invocations *[]int) {
		for i := range *invocations {
			j := rand.Intn(i + 1)
			(*invocations)[i], (*invocations)[j] = (*invocations)[j], (*invocations)[i]
		}
	}

	for minute := range *invocationsEachMinute {
		suffleOneMinute(&(*invocationsEachMinute)[minute])
	}
}

func GenerateExecutionSpecs(function tc.Function) (int, int) {
	var runtime, memory int
	//* Generate a uniform quantiles in [0, 1).
	memQtl := rand.Float64()
	runQtl := rand.Float64()
	runStats := function.RuntimeStats
	memStats := function.MemoryStats

	/**
	 * With 50% prob., returns average values (since we sample the trace based upon the average)
	 * With 50% prob., returns uniform volumns from the the upper and lower percentile interval.
	 */
	if memory = memStats.Average; util.RandBool() {
		switch {
		case memQtl <= 0.01:
			memory = util.RandIntBetween(1, memStats.Percentile1)
		case memQtl <= 0.05:
			memory = util.RandIntBetween(memStats.Percentile1, memStats.Percentile5)
		case memQtl <= 0.25:
			memory = util.RandIntBetween(memStats.Percentile5, memStats.Percentile25)
		case memQtl <= 0.50:
			memory = util.RandIntBetween(memStats.Percentile25, memStats.Percentile50)
		case memQtl <= 0.75:
			memory = util.RandIntBetween(memStats.Percentile50, memStats.Percentile75)
		case memQtl <= 0.95:
			memory = util.RandIntBetween(memStats.Percentile75, memStats.Percentile95)
		case memQtl <= 0.99:
			memory = util.RandIntBetween(memStats.Percentile95, memStats.Percentile99)
		case memQtl < 1:
			memory = util.RandIntBetween(memStats.Percentile99, memStats.Percentile100)
		}
	}

	if runtime = runStats.Average; util.RandBool() {
		switch {
		case runQtl <= 0.01:
			runtime = util.RandIntBetween(runStats.Minimum, runStats.Percentile0)
		case runQtl <= 0.25:
			runtime = util.RandIntBetween(runStats.Percentile1, runStats.Percentile25)
		case runQtl <= 0.50:
			runtime = util.RandIntBetween(runStats.Percentile25, runStats.Percentile50)
		case runQtl <= 0.75:
			runtime = util.RandIntBetween(runStats.Percentile50, runStats.Percentile75)
		case runQtl <= 0.95:
			runtime = util.RandIntBetween(runStats.Percentile75, runStats.Percentile99)
		case runQtl <= 0.99:
			runtime = util.RandIntBetween(runStats.Percentile99, runStats.Percentile100)
		case runQtl < 1:
			// 100%ile is smaller from the max. somehow.
			runtime = util.RandIntBetween(runStats.Percentile100, runStats.Maximum)
		}
	}
	return runtime, memory
}
