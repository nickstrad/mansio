// Baseline JSONL append throughput benchmark for sq-02.
// Measures events/sec and MB/sec for plain Deno.FsFile append writes (no batching).

interface BenchmarkResult {
  sizeBytes: number;
  eventsPerSec: number;
  mbPerSec: number;
  durationMs: number;
}

function buildRecord(targetBytes: number, recordIndex: number): string {
  const ts = new Date().toISOString();
  const level = "info";
  const name = "test-event";
  const msg = "This is a test event";
  const baseLine = JSON.stringify({
    ts,
    level,
    name,
    msg,
    index: recordIndex,
    padding: "",
  }) + "\n";

  // Calculate how much padding we need
  const padLength = Math.max(0, targetBytes - baseLine.length);
  const padding = "x".repeat(padLength);

  const record = JSON.stringify({
    ts,
    level,
    name,
    msg,
    index: recordIndex,
    padding,
  }) + "\n";

  return record;
}

async function runTrial(
  sizeBytes: number,
  numRecords: number,
  trialNum: number,
): Promise<BenchmarkResult> {
  const filePath = `/tmp/append-bench-${sizeBytes}b-trial${trialNum}-${Date.now()}.jsonl`;
  const file = await Deno.open(filePath, {
    write: true,
    append: true,
    create: true,
  });

  try {
    // Warm up: write one record to ensure file is ready
    const warmupRecord = buildRecord(sizeBytes, 0);
    await file.write(new TextEncoder().encode(warmupRecord));

    // Actual benchmark: write numRecords, measuring time
    const startMs = performance.now();
    for (let i = 1; i <= numRecords; i++) {
      const record = buildRecord(sizeBytes, i);
      const encoded = new TextEncoder().encode(record);
      await file.write(encoded);
    }
    const endMs = performance.now();

    const durationMs = endMs - startMs;
    const totalBytes = sizeBytes * (numRecords + 1); // Include warmup
    const totalMb = totalBytes / (1024 * 1024);
    const eventsPerSec = numRecords / (durationMs / 1000);
    const mbPerSec = totalMb / (durationMs / 1000);

    return {
      sizeBytes,
      eventsPerSec: Math.round(eventsPerSec),
      mbPerSec: mbPerSec.toFixed(2),
      durationMs: Math.round(durationMs),
    };
  } finally {
    file.close();
    // Clean up temp file
    try {
      await Deno.remove(filePath);
    } catch {
      // Ignore cleanup errors
    }
  }
}

async function main() {
  console.log("Baseline JSONL append throughput benchmark");
  console.log("==========================================\n");

  const sizes = [200, 1024, 4096];
  const recordCounts: Record<number, number> = {
    200: 20000,
    1024: 4000,
    4096: 1000,
  };

  const results: Record<number, BenchmarkResult[]> = {
    200: [],
    1024: [],
    4096: [],
  };

  // Run 3 trials per size
  for (const size of sizes) {
    const numRecords = recordCounts[size];
    console.log(`Testing ${size}B records (${numRecords} per trial)...`);

    for (let trial = 1; trial <= 3; trial++) {
      const result = await runTrial(size, numRecords, trial);
      results[size].push(result);
      console.log(
        `  Trial ${trial}: ${result.eventsPerSec} events/sec, ${result.mbPerSec} MB/sec (${result.durationMs}ms)`,
      );
    }
  }

  // Calculate medians
  console.log("\n\nResults (median of 3 trials)");
  console.log("=============================\n");

  const medianResults = [];
  for (const size of sizes) {
    const trials = results[size];
    const eventsPerSecValues = trials.map((r) => r.eventsPerSec).sort((a, b) => a - b);
    const mbPerSecValues = trials
      .map((r) => parseFloat(r.mbPerSec as string))
      .sort((a, b) => a - b);

    const medianEventsPerSec = eventsPerSecValues[1]; // Middle of 3 values
    const medianMbPerSec = mbPerSecValues[1];

    medianResults.push({
      sizeBytes: size,
      eventsPerSec: medianEventsPerSec,
      mbPerSec: medianMbPerSec.toFixed(2),
    });

    console.log(`${size}B: ${medianEventsPerSec} events/sec, ${medianMbPerSec.toFixed(2)} MB/sec`);
  }

  console.log("\n\nAcceptance criteria check (item 11: >= 5,000 events/sec for 10,000 events in 2s):");
  for (const result of medianResults) {
    const meetsBar = result.eventsPerSec >= 5000;
    const status = meetsBar ? "✓ PASS" : "✗ FAIL";
    const headroom = result.eventsPerSec - 5000;
    console.log(
      `${result.sizeBytes}B: ${status} (${result.eventsPerSec} events/sec, ${
        headroom >= 0 ? "+" : ""
      }${headroom} headroom)`,
    );
  }
}

main().catch(console.error);
