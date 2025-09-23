# Simple Jump Loss Workers (Python • Rust • Go)

Self‑contained background workers that compute a “simple” jump‑loss estimate for ETH‑USD from the Coinbase WebSocket ticker. All three languages share the same logic and defaults, and each exposes a small library API that spawns its own worker so it won’t interfere with existing bots.

- Source: Coinbase `ETH-USD` ticker (best_bid/best_ask → mid).
- Sampling: 160 ms “buckets”; per bucket return `r = 100 × |ln(last/start)|` (percent).
- Smoothing: EMA of bucket returns `μ_J` with a configurable half‑life (default ~4.8 s).
- Simple JL:
  - If `μ_J > F`: `JL = μ_J − F/2`
  - Else: `JL = μ_J / 2`
- Inputs: `F` is the oracle fee in “thousandths of a basis point” (contract units). Example: `5000 → 0.05%`.
- Reconnects: Exponential backoff with jitter; state persists across reconnects.

## Layout

- `jumplossPython.py` — Python CLI tool
- `jumploss_python_lib.py` — Python library (spawns its own thread + asyncio loop)
- `jumploss-rs/` — Rust crate (library + example binary)
- `jumploss-go/` — Go module with `jumploss` package and example `main`

## Math & Defaults

- Bucket length `Δ` = 160 ms (configurable)
- Half‑life `T½` ≈ 4.8 s (configurable)
- EMA coefficient is derived from half‑life:
  - `alpha = 1 − 2^(−Δ / T½)`
- Default alpha at 160 ms / 4.8 s is ≈ 0.0231

---

## Python

Requirements
- Python 3.9+
- `pip install websockets`

CLI (quick test)
- `python3 jumplossPython.py 5000`
  - Prints periodic lines like: `JL(simple,no-clamp): μ_J=…% F=0.050000% JL=…% buckets=N`

Library (importable, spawns its own worker)
- File: `jumploss_python_lib.py`
- API:
  - `JumpLossWorker(fee_units: int, on_update: Optional[callable] = None, bucket_ms: int = 160, half_life_ms: int = 4800)`
  - Methods: `start_background()`, `stop()`
  - Thread‑safe queue: `worker.updates` yields `JumpLossUpdate(timestamp, mu_j_percent, jl_percent, f_percent, bucket_count)`

Example
```python
import time
from jumploss_python_lib import JumpLossWorker

def on_update(u):
    print(f"JL={u.jl_percent:.6f}%  muJ={u.mu_j_percent:.6f}%  buckets={u.bucket_count}")

worker = JumpLossWorker(
    fee_units=5000,           # F = 0.05%
    on_update=on_update,
    bucket_ms=160,            # optional
    half_life_ms=4800         # optional
)
worker.start_background()
try:
    time.sleep(15)
finally:
    worker.stop()
```

---

## Rust

Requirements
- Rust toolchain (cargo)

CLI (quick test)
- `cd jumploss-rs && cargo run --release -- 5000`

Library
- Add a path dependency from your Rust project (example `Cargo.toml`):
```toml
[dependencies]
jumploss-rs = { path = "../jumploss-rs" }
```
- API:
  - `start_background(fee_units: i64) -> JumpLossHandle` (uses defaults)
  - `start_with_config(fee_units: i64, Config { bucket_ms, half_life_ms }) -> JumpLossHandle`
  - `JumpLossHandle { updates: mpsc::Receiver<JumpLossUpdate>, shutdown: watch::Sender<bool> }`
  - `JumpLossUpdate { timestamp_ms, mu_j_percent, jl_percent, f_percent, bucket_count }`

Example
```rust
use jumploss_rs::{start_with_config, Config};

#[tokio::main]
async fn main() {
    let mut handle = start_with_config(5000, Config { bucket_ms: 160, half_life_ms: 4800 });
    let mut rx = handle.updates;
    while let Some(u) = rx.recv().await {
        println!("JL={:.6}% muJ={:.6}% buckets={} F={:.6}%", u.jl_percent, u.mu_j_percent, u.bucket_count, u.f_percent);
        if u.bucket_count > 50 { let _ = handle.shutdown.send(true); break; }
    }
}
```

---

## Go

Requirements
- Go 1.21+

CLI (quick test)
- `cd jumploss-go && go run . 5000`

Library
- Package: `jumploss-go/jumploss`
- If consuming from another module locally, add a replace in your consumer’s `go.mod`:
```go
require jumploss-go v0.0.0
replace jumploss-go => ../jumploss-go
```
- Import and start:
```go
package main

import (
    "context"
    "fmt"
    "time"
    "jumploss-go/jumploss"
)

func main() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    updates, _ := jumploss.StartWithConfig(ctx, 5000, jumploss.Config{BucketMS: 160, HalfLifeMS: 4800})
    timeout := time.After(15 * time.Second)
    for {
        select {
        case u, ok := <-updates:
            if !ok { return }
            fmt.Printf("JL=%.6f%% muJ=%.6f%% buckets=%d F=%.6f%%\n", u.JLPercent, u.MuJPercent, u.BucketCount, u.FPercent)
        case <-timeout:
            return
        }
    }
}
```

---

## Configuration Summary

- Half‑life and bucket length are configurable in all three libraries.
- Alpha is derived internally per bucket:
  - `alpha = 1 − 2^(−bucket_ms / half_life_ms)`
- Defaults:
  - `bucket_ms = 160`, `half_life_ms = 4800` (≈ 4.8 s)

## Behavior & Notes

- Exponential backoff with jitter on disconnects; state persists across reconnects.
- Buckets continue at the configured cadence; if no new tick in a bucket, return is 0 for that bucket.
- Pair is fixed to `ETH-USD` for now; easy to extend to other products if needed.

## Troubleshooting

- Python: ensure `websockets` is installed; verify outbound WS access.
- Rust: ensure the path dependency points to `jumploss-rs`; run with `--release` for lower CPU.
- Go: ensure Go ≥ 1.21; if importing the package, use a local `replace` to point to `jumploss-go`.

