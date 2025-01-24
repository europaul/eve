// Copyright (c) 2024 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package watcher

import (
	"fmt"
	"github.com/lf-edge/eve/pkg/pillar/types"
	"gonum.org/v1/gonum/stat"
	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
	"image/color"
	"os"
	"path/filepath"
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

func plotMemoryUsage(times, memUsage []float64, redDots []bool, filename string) {
	// Plot memory usage
	if len(times) != len(memUsage) {
		log.Errorf("Mismatched times and memory usage\n")
		return
	}
	// Create a new plot
	p := plot.New()
	p.Title.Text = "Memory Usage"
	p.X.Label.Text = "Time (s)"
	p.Y.Label.Text = "Memory Usage (bytes)"
	// Create a new line plot
	pts := make(plotter.XYs, len(times))
	redPts := make(plotter.XYs, 0)
	for i := range times {
		pts[i].X = times[i]
		pts[i].Y = memUsage[i]
		// if it's a red dot, make it red
		if redDots[i] {
			redPts = append(redPts, pts[i])
		}
	}

	line, err := plotter.NewLine(pts)
	if err != nil {
		log.Errorf("Failed to create line plot: %v\n", err)
		return
	}

	p.Add(line)

	if len(redPts) > 0 {
		scatter, err := plotter.NewScatter(redPts)
		if err != nil {
			log.Errorf("Failed to create scatter plot: %v\n", err)
			return
		}
		scatter.GlyphStyle.Color = color.RGBA{R: 255, A: 255}
		p.Add(scatter)
	}

	// Get a full path for the filename
	filename = filepath.Join(types.MemoryMonitorOutputDir, filename)

	// If file exists, remove it
	if _, err := os.Stat(filename); err == nil {
		if err := os.Remove(filename); err != nil {
			log.Errorf("Failed to remove existing file: %v\n", err)
			return
		}
	}

	// Find the maximum value to set the Y axis limit

	if err := p.Save(40*vg.Inch, 20*vg.Inch, filename); err != nil {
		log.Errorf("Failed to save plot: %v\n", err)
	}

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
		var redDots []bool

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
				redDots = append(redDots, false)

				// Keep only the last 'sampleSize' samples
				if len(times) > sampleSize {
					times = times[len(times)-sampleSize:]
					heapValues = heapValues[len(heapValues)-sampleSize:]
					RSSValues = RSSValues[len(RSSValues)-sampleSize:]
					redDots = redDots[len(redDots)-sampleSize:]
				}

				// Only run regression if we have enough samples
				if len(times) > minimalSampleSize {
					// Get a timestamp for the filename
					// Smooth the values via a median filter to reduce spikes
					smoothedHeapValues := medianFilter(heapValues, smoothWindowSize)
					smoothedRSSValues := medianFilter(RSSValues, smoothWindowSize)
					heapSlope := linearRegressionSlope(times, smoothedHeapValues)
					RSSSlope := linearRegressionSlope(times, smoothedRSSValues)
					// If slope is positive and above a certain threshold, print a warning
					if heapSlope > threshold {
						log.Tracef("Potential heap memory leak detected: slope %.2f > %.2f\n", heapSlope, threshold)
						log.Warnf("Potential heap memory leak detected: slope %.2f > %.2f\n", heapSlope, threshold)
						plotMemoryUsage(times, heapValues, redDots, "heap_usage.png")
						redDots[len(redDots)-1] = true
					}
					if RSSSlope > threshold {
						log.Tracef("Potential RSS memory leak detected: slope %.2f > %.2f\n", RSSSlope, threshold)
						log.Warnf("Potential RSS memory leak detected: slope %.2f > %.2f\n", RSSSlope, threshold)
						plotMemoryUsage(times, RSSValues, redDots, "RSS_usage.png")
						redDots[len(redDots)-1] = true
					}
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
