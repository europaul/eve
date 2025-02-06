// Copyright (c) 2024 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package watcher

import (
	"encoding/csv"
	"fmt"
	"github.com/lf-edge/eve/pkg/pillar/types"
	"gonum.org/v1/gonum/stat"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"
)

var (
	// MemLeakMinimumInterval is the minimum time interval on which we can detect a memory leak
	MemLeakMinimumInterval = 10 * time.Minute
	// SmoothInterval is the window size for the median filter to reduce spikes
	SmoothInterval = 10 * time.Second
)

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

		// Create a temporary output file
		tmp, err := os.CreateTemp("", "truncated-*.csv")
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
			if err := writer.Write(record); err != nil {
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

// writeMemoryUsage writes out times, memUsage, and redDots as CSV.
// There's no repeated field name, so it's smaller than typical JSON.
// Then you can load this CSV in any interactive plotting tool.
func writeMemoryUsage(timeNow time.Time, memUsage uint64, outPath string) {

	// Open file in append mode, create if it doesn't exist
	file, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Error opening file: %v\n", err)
		return
	}
	defer controlFileSize(outPath, 1024)
	defer file.Close()

	w := csv.NewWriter(file)

	// Check if file is empty to write header
	fileInfo, err := file.Stat()
	if err != nil {
		fmt.Printf("Error getting file info: %v\n", err)
		return
	}

	if fileInfo.Size() == 0 {
		header := []string{"time", "memory_usage"}
		if err := w.Write(header); err != nil {
			fmt.Printf("Error writing header: %v\n", err)
			return
		}
	}

	// Write the data
	record := []string{timeNow.Format(time.RFC3339), fmt.Sprintf("%d", memUsage)}
	if err := w.Write(record); err != nil {
		fmt.Printf("Error writing record: %v\n", err)
		return
	}

	// Flush any buffered data to disk
	w.Flush()
	if err := w.Error(); err != nil {
		fmt.Printf("Error flushing CSV writer: %v\n", err)
		return
	}

	fmt.Printf("Data exported to %s\n", outPath)
}

func readFirstStatAvailable(filename string) *time.Time {
	path := filepath.Join(types.MemoryMonitorOutputDir, filename)
	file, err := os.Open(path)
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

func readSamplesOlderThan(filename string, timeCutoff time.Time) []uint64 {
	path := filepath.Join(types.MemoryMonitorOutputDir, filename)
	file, err := os.Open(path)
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

	var values []uint64
	var firstFound bool
	for _, record := range records[1:] {
		timeStr := record[0]
		if !firstFound {
			timeRead, err := time.Parse(time.RFC3339, timeStr)
			if err != nil {
				log.Warnf("failed to parse time: %v", err)
				continue
			}
			if timeRead.Before(timeCutoff) {
				firstFound = true
			}
		}
		valueStr := record[1]
		value, err := strconv.ParseUint(valueStr, 10, 64)
		if err != nil {
			log.Warnf("failed to parse value: %v", err)
			continue
		}
		values = append(values, value)
	}

	return values
}

// This goroutine periodically captures memory usage stats and attempts to detect
// a potential memory leak by looking at the trend of heap usage over time. It uses a
// simple linear regression to estimate whether heap memory usage is consistently rising.
//
// Note that this is a simplistic heuristic and can produce false positives or fail to detect
// subtle leaks. In a real-world scenario, you'd likely want more robust logic or integrate
// with profiling tools.

// MemoryMonitor starts a goroutine that periodically samples memory usage,
// applies a simple linear regression to recent samples, and if the slope is
// consistently positive and above a threshold, it considers it a potential leak.
func MemoryMonitor(interval time.Duration, sampleSize int, threshold float64) chan struct{} {
	log.Tracef("Starting memory leak detector with interval %v, sample size %d, threshold %.2f\n", interval, sampleSize, threshold)
	stopCh := make(chan struct{})
	go func() {
		statsPointsNum := 0

		//smoothWindowSize := int(SmoothInterval / interval)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				// Gather memory stats
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				heapInUse := m.HeapInuse
				rss, err := getRSS()
				if err != nil {
					log.Errorf("Failed to read RSS: %v\n", err)
					continue
				}
				log.Tracef("Heap usage: %d MB, RSS: %.2f MB\n", heapInUse/1024/1024, float64(rss)/1024/1024)

				timeNow := time.Now()

				// Append new sample
				statsPointsNum++

				// Write memory usage to CSV
				fileName := path.Join(types.MemoryMonitorOutputDir, "heap_usage.csv")
				writeMemoryUsage(timeNow, heapInUse, fileName)
				fileName = path.Join(types.MemoryMonitorOutputDir, "rss_usage.csv")
				writeMemoryUsage(timeNow, rss, fileName)

				firstStatAvailable := readFirstStatAvailable("heap_usage.csv")
				if firstStatAvailable == nil {
					log.Warnf("No stats available, skipping regression\n")
					continue
				}

				// If we have enough samples, perform regression
				if timeNow.Sub(*firstStatAvailable) < MemLeakMinimumInterval {

					// Calculate the time of the first sample to be used in the regression
					//firstSampleTime := timeNow.Add(-MemLeakMinimumInterval)

					// Read all samples older than the first sample time
					//heapValues := readSamplesOlderThan("heap_usage.csv", firstSampleTime)
					//rssValues := readSamplesOlderThan("rss_usage.csv", firstSampleTime)

					// Get a timestamp for the filename
					// Smooth the values via a median filter to reduce spikes
					// smoothedHeapValues := medianFilter(heapValues, smoothWindowSize)
					// smoothedRSSValues := medianFilter(rssValues, smoothWindowSize)
					// heapSlope := linearRegressionSlope(times, smoothedHeapValues)
					// RSSSlope := linearRegressionSlope(times, smoothedRSSValues)
					// If slope is positive and above a certain threshold, print a warning
					//if heapSlope > threshold {
					//	log.Tracef("Potential heap memory leak detected: slope %.2f > %.2f\n", heapSlope, threshold)
					//	log.Warnf("Potential heap memory leak detected: slope %.2f > %.2f\n", heapSlope, threshold)
					//}
					//if RSSSlope > threshold {
					//	log.Tracef("Potential RSS memory leak detected: slope %.2f > %.2f\n", RSSSlope, threshold)
					//	log.Warnf("Potential RSS memory leak detected: slope %.2f > %.2f\n", RSSSlope, threshold)
					//}
				}
			}
		}
	}()
	return stopCh
}

// linearRegressionSlope calculates the slope (beta) via Gonum's LinearRegression.
func linearRegressionSlope(xs, ys []float64) float64 {
	// Basic sanity check
	if len(xs) != len(ys) || len(xs) < 2 {
		return 0
	}
	// LinearRegression returns (alpha, beta)
	alpha, beta := stat.LinearRegression(xs, ys, nil, false)
	_ = alpha // We only need slope in this case
	return beta
}
