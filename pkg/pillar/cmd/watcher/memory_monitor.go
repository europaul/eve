// Copyright (c) 2024 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package watcher

import (
	"fmt"
	"os"
	"runtime"
	"sort"
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
func medianFilter(values []float64, windowSize int) []float64 {
	if windowSize < 1 {
		return values
	}
	if windowSize == 1 || windowSize > len(values) {
		// No filtering possible if window size is 1 or larger than data
		return values
	}
	out := make([]float64, len(values))
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
		window := append([]float64{}, values[start:end+1]...)
		sort.Float64s(window)
		mid := len(window) / 2
		out[i] = window[mid]
	}
	return out
}

// getRSS reads /proc/self/statm and returns the RSS in bytes.
// Format of /proc/self/statm: size resident share text lib data dt
// We're interested in the second field: 'resident'.
func getRSS() (int64, error) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	var size, rss int64
	if _, err := fmt.Sscanf(string(data), "%d %d", &size, &rss); err != nil {
		return 0, err
	}
	pageSize := int64(os.Getpagesize())
	return rss * pageSize, nil
}

// This goroutine periodically captures memory usage stats and attempts to detect
// a potential memory leak by looking at the trend of heap usage over time. It uses a
// simple linear regression to estimate whether heap memory usage is consistently rising.
//
// Note that this is a simplistic heuristic and can produce false positives or fail to detect
// subtle leaks. In a real-world scenario, you'd likely want more robust logic or integrate
// with profiling tools.

// MemoryLeakDetector starts a goroutine that periodically samples memory usage,
// applies a simple linear regression to recent samples, and if the slope is
// consistently positive and above a threshold, it considers it a potential leak.
func MemoryLeakDetector(interval time.Duration, sampleSize int, threshold float64) chan struct{} {
	log.Tracef("Starting memory leak detector with interval %v, sample size %d, threshold %.2f\n", interval, sampleSize, threshold)
	stopCh := make(chan struct{})
	go func() {
		var times []float64
		var heapValues []float64
		var RSSValues []float64

		smoothWindowSize := int(SmoothInterval / interval)
		minimalSampleSize := int(MemLeakMinimumInterval / interval)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		start := time.Now()
		for {
			select {
			case <-stopCh:
				return
			case t := <-ticker.C:
				// Gather memory stats
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				heapInUse := float64(m.HeapInuse)
				rss, err := getRSS()
				if err != nil {
					log.Errorf("Failed to read RSS: %v\n", err)
					continue
				}
				log.Tracef("Heap usage: %.2f MB, RSS: %.2f MB\n", heapInUse/1024/1024, float64(rss)/1024/1024)

				// Append new sample
				elapsed := t.Sub(start).Seconds()
				times = append(times, elapsed)
				heapValues = append(heapValues, heapInUse)
				RSSValues = append(RSSValues, float64(rss))

				// Keep only the last 'sampleSize' samples
				if len(times) > sampleSize {
					times = times[len(times)-sampleSize:]
					heapValues = heapValues[len(heapValues)-sampleSize:]
					RSSValues = RSSValues[len(RSSValues)-sampleSize:]
				}

				// Only run regression if we have enough samples
				if len(times) > minimalSampleSize {
					// Smooth the values via a median filter to reduce spikes
					smoothedHeapValues := medianFilter(heapValues, smoothWindowSize)
					smoothedRSSValues := medianFilter(RSSValues, smoothWindowSize)
					heapSlope := linearRegressionSlope(times, smoothedHeapValues)
					RSSSlope := linearRegressionSlope(times, smoothedRSSValues)
					// If slope is positive and above a certain threshold, print a warning
					if heapSlope > threshold {
						log.Tracef("Potential memory leak detected: slope %.2f > %.2f\n", heapSlope, threshold)
					}
					if RSSSlope > threshold {
						log.Tracef("Potential RSS memory leak detected: slope %.2f > %.2f\n", RSSSlope, threshold)
					}
				}
			}
		}
	}()
	return stopCh
}

// linearRegressionSlope calculates the slope (m) of the best-fit line for the given data points.
// Using the formula for simple linear regression with one independent variable.
// slope = sum((x - meanX)*(y - meanY)) / sum((x - meanX)^2)
func linearRegressionSlope(xs, ys []float64) float64 {
	if len(xs) != len(ys) {
		return 0.0
	}
	n := float64(len(xs))
	var sumX, sumY, sumXY, sumX2 float64
	for i := 0; i < len(xs); i++ {
		sumX += xs[i]
		sumY += ys[i]
		sumXY += xs[i] * ys[i]
		sumX2 += xs[i] * xs[i]
	}

	meanX := sumX / n
	meanY := sumY / n

	numerator := 0.0
	denominator := 0.0
	for i := 0; i < len(xs); i++ {
		xDiff := xs[i] - meanX
		yDiff := ys[i] - meanY
		numerator += xDiff * yDiff
		denominator += xDiff * xDiff
	}

	if denominator == 0 {
		return 0.0
	}
	return numerator / denominator
}
