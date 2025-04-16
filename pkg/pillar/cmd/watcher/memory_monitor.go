// Copyright (c) 2025 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package watcher

import (
	"context"
	"encoding/csv"
	"fmt"
	"github.com/lf-edge/eve/pkg/pillar/agentlog"
	"github.com/lf-edge/eve/pkg/pillar/types"
	"gonum.org/v1/gonum/stat"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"
)

var (
	heapReachedThreshold int
	rssReachedThreshold  int
)

// InternalMemoryMonitor is a struct that holds the parameters for the memory monitor.
type InternalMemoryMonitorParams struct {
	mutex               sync.Mutex
	analysisPeriod      time.Duration
	smoothingProbeCount int
	probingInterval     time.Duration
	slopeThreshold      float64
	isActive            bool
	// Context to make the monitoring goroutine cancellable
	context context.Context
	stop    context.CancelFunc
}

func (immp *InternalMemoryMonitorParams) Set(analysisPeriod, probingInterval time.Duration, slopeThreshold float64, smoothingProbeCount int, isActive bool) {
	immp.mutex.Lock()
	immp.analysisPeriod = analysisPeriod
	immp.smoothingProbeCount = smoothingProbeCount
	immp.probingInterval = probingInterval
	immp.slopeThreshold = slopeThreshold
	immp.isActive = isActive
	immp.mutex.Unlock()
}

func (immp *InternalMemoryMonitorParams) Get() (time.Duration, time.Duration, float64, int, bool) {
	immp.mutex.Lock()
	defer immp.mutex.Unlock()
	return immp.analysisPeriod, immp.probingInterval, immp.slopeThreshold, immp.smoothingProbeCount, immp.isActive
}

func (immp *InternalMemoryMonitorParams) MakeStoppable() {
	immp.context, immp.stop = context.WithCancel(context.Background())
}

func (immp *InternalMemoryMonitorParams) isStoppable() bool {
	if immp.context == nil {
		return false
	}
	return immp.context.Err() == nil
}

func (immp *InternalMemoryMonitorParams) checkStopCondition() bool {
	immp.mutex.Lock()
	defer immp.mutex.Unlock()
	if immp.context == nil {
		return false
	}
	select {
	case <-immp.context.Done():
		return true
	default:
		return false
	}
}

func (immp *InternalMemoryMonitorParams) Stop() {
	if immp.stop != nil {
		immp.stop()
	}
}

// medianFilter applies a median filter to a slice of values. windowSize should be odd.
// This reduces the impact of local spikes.
func medianFilter(values []uint64, windowSize int) []uint64 {
	if windowSize < 1 {
		return values
	}
	if windowSize == 1 || windowSize > len(values) {
		// No filtering possible if window size is 1 or larger than data
		return values
	}
	out := make([]uint64, len(values))
	half := windowSize / 2
	for i := range values {
		start := i - half
		end := i + half
		if start < 0 {
			start = 0
		}
		if end >= len(values) {
			end = len(values) - 1
		}
		window := append([]uint64{}, values[start:end+1]...)
		sort.Slice(window, func(i, j int) bool { return window[i] < window[j] })
		mid := len(window) / 2
		out[i] = window[mid]
	}
	return out
}

// getRSS reads /proc/self/statm and returns the RSS in bytes.
// Format of /proc/self/statm: size resident share text lib data dt
// We're interested in the second field: 'resident'.
func getRSS() (uint64, error) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	var size, rss uint64
	if _, err := fmt.Sscanf(string(data), "%d %d", &size, &rss); err != nil {
		return 0, err
	}
	pageSize := uint64(os.Getpagesize())
	return rss * pageSize, nil
}

func controlFileSize(filename string, threshold int64) {
	const linesToRemove = 12

	// Keep trimming until size <= threshold (or until we can't trim anymore).
	for {

		// Open the file
		file, err := os.OpenFile(filename, os.O_RDWR, 0644)
		if err != nil {
			log.Warnf("failed to open file: %v", err)
			return
		}

		fi, err := file.Stat()
		if err != nil {
			log.Warnf("failed to get file info: %v", err)
			file.Close()
			return
		}

		// If file size is okay, done
		if fi.Size() <= threshold {
			file.Close()
			return
		}

		fileDir := filepath.Dir(filename)

		// Create a temporary output file
		tmp, err := os.CreateTemp(fileDir, "truncated-*.csv")
		if err != nil {
			log.Warnf("failed to create temp file: %v", err)
			file.Close()
			return
		}

		renamed := false
		deferCleanup := func() {
			tmp.Close()
			if !renamed {
				os.Remove(tmp.Name())
			}
		}

		// We’ll use a named cleanup so we can call it as needed.
		// We won't just do `defer deferCleanup()` right away,
		// because we might close early.
		// Instead, call `deferCleanup()` on any return path.

		reader := csv.NewReader(file)
		writer := csv.NewWriter(tmp)

		// 1) read header
		header, err := reader.Read()
		if err != nil {
			log.Warnf("failed to read header: %v", err)
			file.Close()
			deferCleanup()
			return
		}

		if err := writer.Write(header); err != nil {
			log.Warnf("failed to write header: %v", err)
			file.Close()
			deferCleanup()
			return
		}

		// 2) skip lines
		for i := 0; i < linesToRemove; i++ {
			_, err := reader.Read()
			if err == io.EOF {
				// End of file, nothing else to skip
				break
			}
			if err != nil {
				log.Warnf("error reading csv: %v", err)
				file.Close()
				deferCleanup()
				return
			}
		}

		// 3) copy everything else
		for {
			record, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Warnf("error reading csv: %v", err)
				file.Close()
				deferCleanup()
				return
			}
			if err = writer.Write(record); err != nil {
				log.Warnf("error writing csv: %v", err)
				file.Close()
				deferCleanup()
				return
			}
		}

		// Flush writer
		writer.Flush()
		if err := writer.Error(); err != nil {
			log.Warnf("error flushing csv writer: %v", err)
			file.Close()
			deferCleanup()
			return
		}

		// Close original file
		if err := file.Close(); err != nil {
			log.Warnf("error closing original file: %v", err)
			deferCleanup()
			return
		}

		// Close temp file
		if err := tmp.Close(); err != nil {
			log.Warnf("error closing temp file: %v", err)
			return
		}

		// Overwrite original with truncated version
		if err := os.Rename(tmp.Name(), filename); err != nil {
			log.Warnf("error renaming temp file: %v", err)
			os.Remove(tmp.Name()) // we can do an explicit remove here
			return
		}

		// Mark rename success, so we don't remove the file in defer.
		renamed = true

		// Continue the loop to see if we need more trimming.
		// If we want to do it in multiple passes (again and again),
		// just loop around. If removing lines once is enough, then
		// we break here instead.
	}
}

type memoryUsage struct {
	usage uint64
	name  string
}

// writeMemoryUsage writes out times, memUsage, and redDots as CSV.
// There's no repeated field name, so it's smaller than typical JSON.
// Then you can load this CSV in any interactive plotting tool.
func writeMemoryUsage(timeNow time.Time, memUsage []memoryUsage, outPath string) {

	// Open file in append mode, create if it doesn't exist
	file, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Error opening file: %v\n", err)
		return
	}
	defer controlFileSize(outPath, 1024*1024) // 1MB
	defer file.Close()

	w := csv.NewWriter(file)

	// Check if file is empty to write header
	fileInfo, err := file.Stat()
	if err != nil {
		fmt.Printf("Error getting file info: %v\n", err)
		return
	}

	if fileInfo.Size() == 0 {
		// Form header as "time,memory_usage_name1, memory_usage_name2, ... ,red"
		header := make([]string, 0, len(memUsage)+2)
		header = append(header, "time")
		for _, mu := range memUsage {
			header = append(header, mu.name)
		}
		header = append(header, "red")
		if err = w.Write(header); err != nil {
			fmt.Printf("Error writing header: %v\n", err)
			return
		}
	}

	record := make([]string, 0, len(memUsage)+2)
	record = append(record, timeNow.Format(time.RFC3339))
	for _, mu := range memUsage {
		record = append(record, strconv.FormatUint(mu.usage, 10))
	}
	record = append(record, "0") // Default red value

	// Write the data
	if err = w.Write(record); err != nil {
		fmt.Printf("Error writing record: %v\n", err)
		return
	}

	// Flush any buffered data to disk
	w.Flush()
	if err = w.Error(); err != nil {
		fmt.Printf("Error flushing CSV writer: %v\n", err)
		return
	}

	fmt.Printf("Data exported to %s\n", outPath)
}

// markLastUsageRed marks the last usage in the file as red.
func markLastUsageRed(filename string) {
	// Open the file
	file, err := os.OpenFile(filename, os.O_RDWR, 0644)
	if err != nil {
		log.Warnf("failed to open file: %v", err)
		return
	}
	defer file.Close()

	// Seek to the end of the file
	offset, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		fmt.Println("Error seeking file:", err)
		return
	}

	// Read the file backwards until we find the last "0"
	// and replace it with "1"
	for {
		offset--
		if offset < 0 {
			fmt.Println("Reached beginning of file without finding 0")
			return
		}
		_, err = file.Seek(offset, io.SeekStart)
		if err != nil {
			fmt.Println("Error seeking file:", err)
			return
		}
		b := make([]byte, 1)
		_, err = file.Read(b)
		if err != nil {
			fmt.Println("Error reading file:", err)
			return
		}
		if b[0] == '0' {
			break
		}
	}

	_, err = file.WriteAt([]byte("1"), offset)
	if err != nil {
		fmt.Println("Error writing file:", err)
		return
	}

	fmt.Println("Last usage marked as red")

	// Flush any buffered data to disk
	if err := file.Sync(); err != nil {
		fmt.Printf("Error flushing file: %v\n", err)
		return
	}
	return
}

func readFirstStatAvailable(filename string) *time.Time {
	filePath := filepath.Join(types.MemoryMonitorOutputDir, filename)
	file, err := os.Open(filePath)
	if err != nil {
		log.Warnf("failed to open file: %v", err)
		return nil
	}
	defer file.Close()

	r := csv.NewReader(file)
	records, err := r.ReadAll()
	if err != nil {
		log.Warnf("failed to read csv: %v", err)
		return nil
	}

	if len(records) < 2 {
		log.Warnf("no records found in file")
		return nil
	}

	// Parse the first record after the header
	timeStr := records[1][0]
	timeRead, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		log.Warnf("failed to parse time: %v", err)
		return nil
	}

	return &timeRead
}

func readSamplesNewerThan(filename string, timeCutoff time.Time) []memoryUsage {
	filePath := filepath.Join(types.MemoryMonitorOutputDir, filename)
	file, err := os.Open(filePath)
	if err != nil {
		log.Warnf("failed to open file: %v", err)
		return nil
	}
	defer file.Close()

	r := csv.NewReader(file)
	records, err := r.ReadAll()
	if err != nil {
		log.Warnf("failed to read csv: %v", err)
		return nil
	}

	if len(records) < 2 {
		log.Warnf("no records found in file")
		return nil
	}

	// Parse the header to fill the names
	header := records[0]
	names := make([]string, len(header)-2) // Exclude time and red
	for i := 1; i < len(header)-1; i++ {
		names[i-1] = header[i]
	}

	var values []memoryUsage
	for _, record := range records[1:] {
		timeStr := record[0]
		timeRead, err := time.Parse(time.RFC3339, timeStr)
		if err != nil {
			log.Warnf("failed to parse time: %v", err)
			continue
		}
		if timeRead.Before(timeCutoff) {
			continue
		}
		// Parse the values for all the names in the header
		for i := 1; i < len(record)-1; i++ {
			value, err := strconv.ParseUint(record[i], 10, 64)
			if err != nil {
				log.Warnf("failed to parse value: %v", err)
				continue
			}
			values = append(values, memoryUsage{usage: value, name: names[i-1]})
		}
	}

	return values
}

func performMemoryLeakDetection(analysisPeriod time.Duration, probingInterval time.Duration, slopeThreshold float64, smoothingProbeCount int) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	heapInUse := m.HeapInuse
	rss, err := getRSS()
	if err != nil {
		log.Errorf("Failed to read RSS: %v\n", err)
		return
	}
	log.Tracef("Heap usage: %d MB, RSS: %.2f MB\n", heapInUse/1024/1024, float64(rss)/1024/1024)

	timeNow := time.Now()

	// Write memory usage to CSV
	fileName := path.Join(types.MemoryMonitorOutputDir, "memory_usage.csv")
	memUsage := []memoryUsage{
		{usage: heapInUse, name: "heap"},
		{usage: rss, name: "rss"},
	}
	writeMemoryUsage(timeNow, memUsage, fileName)
	firstStatAvailable := readFirstStatAvailable(fileName)

	// If we have enough samples, perform regression
	if timeNow.Sub(*firstStatAvailable) >= analysisPeriod {

		// Calculate the time of the first sample to be used in the regression
		firstSampleTime := timeNow.Add(analysisPeriod * -1)

		// Read all samples newer than the first sample time
		usageValues := readSamplesNewerThan(fileName, firstSampleTime)

		heapValues := make([]uint64, 0)
		rssValues := make([]uint64, 0)
		for _, mu := range usageValues {
			if mu.name == "heap" {
				heapValues = append(heapValues, mu.usage)
			}
			if mu.name == "rss" {
				rssValues = append(rssValues, mu.usage)
			}
		}

		// Get a timestamp for the filename
		// Smooth the values via a median filter to reduce spikes
		smoothedHeapValues := medianFilter(heapValues, smoothingProbeCount)
		smoothedRSSValues := medianFilter(rssValues, smoothingProbeCount)
		times := make([]float64, len(smoothedHeapValues))
		for i := range smoothedHeapValues {
			times[i] = float64(i) * probingInterval.Seconds()
		}

		heapSlope := linearRegressionSlope(times, smoothedHeapValues)
		RSSSlope := linearRegressionSlope(times, smoothedRSSValues)
		//If slope is positive and above a certain threshold, print a warning
		if heapSlope > slopeThreshold {
			heapReachedThreshold++
			if heapReachedThreshold > 10 {
				log.Warnf("Potential memory leak (heap) detected: slope %.2f > %.2f\n", heapSlope, slopeThreshold)
				markLastUsageRed(fileName)
			}
		} else {
			heapReachedThreshold = 0
		}
		if RSSSlope > slopeThreshold {
			rssReachedThreshold++
			if rssReachedThreshold > 10 {
				log.Warnf("Potential memory leak (RSS) detected: slope %.2f > %.2f\n", RSSSlope, slopeThreshold)
				markLastUsageRed(fileName)
			}
		} else {
			rssReachedThreshold = 0
		}
	}

}

// This goroutine periodically captures memory usage stats and attempts to detect
// a potential memory leak by looking at the trend of heap usage over time. It uses a
// simple linear regression to estimate whether heap memory usage is consistently rising.
//
// Note that this is a simplistic heuristic and can produce false positives or fail to detect
// subtle leaks. In a real-world scenario, you'd likely want more robust logic or integrate
// with profiling tools.

func InternalMemoryMonitor(ctx *watcherContext) {
	log.Functionf("Starting internal memory monitor (stoppable: %v)", ctx.IMMParams.isStoppable())
	log.Tracef("Starting internal memory monitor")
	// Get the initial memory leak detection parameters
	for {
		analysisPeriod, probingInterval, slopeThreshold, smoothingProbeCount, isActive := ctx.IMMParams.Get()
		// Check if we have to stop
		if ctx.IMMParams.checkStopCondition() {
			log.Functionf("Stopping internal memory monitor")
			return
		}
		if !isActive {
			performMemoryLeakDetection(analysisPeriod, probingInterval, slopeThreshold, smoothingProbeCount)
		}

		// Sleep for the probing interval
		time.Sleep(probingInterval)
	}
}

// linearRegressionSlope calculates the slope (beta) via Gonum's LinearRegression.
func linearRegressionSlope(xs []float64, ys []uint64) float64 {
	// Basic sanity check
	if len(xs) != len(ys) || len(xs) < 2 {
		log.Warnf("#ohm: Insufficient data for regression\n")
		return 0
	}
	// Convert uint64 to float64
	ysFloat := make([]float64, len(ys))
	for i, y := range ys {
		ysFloat[i] = float64(y)
	}
	// LinearRegression returns (alpha, beta)
	alpha, beta := stat.LinearRegression(xs, ysFloat, nil, false)
	log.Noticef("#ohm: Linear regression: alpha %.2f, beta %.2f\n", alpha, beta)
	return beta
}

func updateInternalMemoryMonitorConfig(ctx *watcherContext) {
	gcp := agentlog.GetGlobalConfig(log, ctx.subGlobalConfig)
	if gcp == nil {
		return
	}

	// Update the internal memory monitor parameters
	analysisPeriod := time.Duration(gcp.GlobalValueInt(types.InternalMemoryMonitorAnalysisPeriodMinutes)) * time.Minute
	probingInterval := time.Duration(gcp.GlobalValueInt(types.InternalMemoryMonitorProbingIntervalSeconds)) * time.Second
	slopeThreshold := float64(gcp.GlobalValueInt(types.InternalMemoryMonitorSlopeThreshold))
	smoothingPeriod := time.Duration(gcp.GlobalValueInt(types.InternalMemoryMonitorSmoothingPeriodSeconds)) * time.Second
	smoothingProbeCount := int(smoothingPeriod / probingInterval)
	isActive := gcp.GlobalValueBool(types.InternalMemoryMonitorEnabled)
	ctx.IMMParams.Set(analysisPeriod, probingInterval, slopeThreshold, smoothingProbeCount, isActive)
}
