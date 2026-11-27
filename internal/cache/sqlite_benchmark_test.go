package cache

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	sqliteBenchmarkEntryCount  = 64
	sqliteBenchmarkPayloadSize = 1024
	sqliteBenchmarkReaders     = 32
	sqliteBenchmarkVersion     = "benchmark-version"
)

func BenchmarkSQLiteMetadataReads(b *testing.B) {
	benchmarkSQLiteMetadataReads(b, false)
}

func BenchmarkSQLiteMetadataReadsWithWriter(b *testing.B) {
	benchmarkSQLiteMetadataReads(b, true)
}

func benchmarkSQLiteMetadataReads(b *testing.B, withWriter bool) {
	ctx, _, _, service := newFilesystemBenchmarkService(b)
	payload := bytes.Repeat([]byte{0x7e}, sqliteBenchmarkPayloadSize)
	keys := make([]string, sqliteBenchmarkEntryCount)
	for index := range keys {
		keys[index] = fmt.Sprintf("sqlite-metadata-%d", index)
		if err := saveSQLiteBenchmarkEntry(ctx, service, keys[index], payload); err != nil {
			b.Fatal(err)
		}
	}

	readLatencies := make([]int64, min(b.N, benchmarkSampleLimit))
	var readSamples atomic.Uint64
	var readerOffset atomic.Uint64
	var firstErr error
	var errOnce sync.Once
	fail := func(err error) { errOnce.Do(func() { firstErr = err }) }

	var writeLatencies []int64
	stopWriter := make(chan struct{})
	var writerWG sync.WaitGroup
	if withWriter {
		writerWG.Go(func() {
			for iteration := 0; ; iteration++ {
				select {
				case <-stopWriter:
					return
				default:
				}
				started := time.Now()
				if err := saveSQLiteBenchmarkEntry(ctx, service, fmt.Sprintf("sqlite-writer-%d", iteration), payload); err != nil {
					fail(fmt.Errorf("background writer: %w", err))
					return
				}
				if len(writeLatencies) < benchmarkSampleLimit {
					writeLatencies = append(writeLatencies, time.Since(started).Nanoseconds())
				}
			}
		})
	}

	procs := runtime.GOMAXPROCS(0)
	parallelism := max(1, (sqliteBenchmarkReaders+procs/2)/procs)
	scope := benchmarkScope()
	fallbackURL := func(cacheEntryID string) string { return "/download/" + cacheEntryID }

	b.ReportAllocs()
	b.SetParallelism(parallelism)
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		next := int(readerOffset.Add(1))
		for parallel.Next() {
			key := keys[next%len(keys)]
			next++
			started := time.Now()
			match, err := service.GetCacheEntryWithDownloadURL(ctx, []string{key}, sqliteBenchmarkVersion, scope, fallbackURL)
			elapsed := time.Since(started).Nanoseconds()
			if err == nil && (match == nil || match.CacheEntry.Key != key) {
				err = fmt.Errorf("cache entry %q was not matched", key)
			}
			if err != nil {
				fail(err)
			}
			if index := readSamples.Add(1) - 1; index < uint64(len(readLatencies)) {
				readLatencies[index] = elapsed
			}
		}
	})
	b.StopTimer()
	close(stopWriter)
	writerWG.Wait()

	if firstErr != nil {
		b.Fatal(firstErr)
	}
	reportSQLiteBenchmarkP95(b, readLatencies, "p95-ns")
	if withWriter {
		reportSQLiteBenchmarkP95(b, writeLatencies, "write-p95-ns")
		b.ReportMetric(float64(len(writeLatencies)), "writes")
	}
	b.ReportMetric(float64(parallelism*procs), "readers")
}

func saveSQLiteBenchmarkEntry(ctx context.Context, service *Service, key string, payload []byte) error {
	scope := benchmarkScope()
	upload, err := service.CreateUpload(ctx, key, sqliteBenchmarkVersion, scope)
	if err != nil {
		return fmt.Errorf("create upload %q: %w", key, err)
	}
	if err := service.UploadPart(ctx, upload.UploadID, bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("upload part %q: %w", key, err)
	}
	if _, err := service.CompleteUpload(ctx, key, sqliteBenchmarkVersion, scope); err != nil {
		return fmt.Errorf("complete upload %q: %w", key, err)
	}
	return nil
}

func reportSQLiteBenchmarkP95(b *testing.B, latencies []int64, unit string) {
	if len(latencies) == 0 {
		return
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	b.ReportMetric(float64(latencies[(len(latencies)*95+99)/100-1]), unit)
}
