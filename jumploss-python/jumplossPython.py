#!/usr/bin/env python3
"""
Simple Jump Loss (Python)

Run: python3 jumplossPython.py 5000
 - Argument is oracle fee in thousandths of a basis point (same units as the contract).
 - Example 5000 -> F = 0.05% (5 bps).

This script connects to Coinbase ETH-USD ticker, forms 160ms buckets of mid-price
changes, maintains an EMA of absolute bucket returns (μ_J in percent), and computes
the simple jump loss using the piecewise rule WITHOUT clamping:

  if μ_J > F:   JL = μ_J - F/2
  else:         JL = μ_J / 2

No additional clamp to [0, μ_J] and no 0.5% cap are applied.
"""

import sys
import json
import math
import time
import asyncio
import random
import websockets
import websockets.exceptions

COINBASE_WS = "wss://ws-feed.exchange.coinbase.com"

# 160ms buckets and EMA config (matches UI logic)
BUCKET_MS = 160
ALPHA_160MS = 0.0231  # ~30-bucket half-life
MIN_BUCKETS = 5       # must accumulate before reporting


def units_to_percent(units: int) -> float:
    """Convert thousandths-of-basis-points to percent.
    Example: 5000 -> 0.05%.
    Contract precision is 1e7, so: percent = (units / 1e7) * 100.
    """
    return (units / 1e7) * 100.0


def simple_jump_loss(muJ_percent: float, F_percent: float) -> float:
    """Piecewise jump loss WITHOUT clamping/capping."""
    if muJ_percent > F_percent:
        return muJ_percent - (F_percent / 2.0)
    else:
        return muJ_percent / 2.0


async def run(F_units: int):
    F_percent = units_to_percent(F_units)
    print(f"📈 Using barrier F = {F_percent:.6f}% (from {F_units} units)")

    mean_tick_jump_ema = 0.0  # μ_J (percent)
    bucket_start_time = None
    bucket_start_price = None
    bucket_last_price = None
    bucket_count = 0
    last_print = 0.0

    attempt = 0  # reconnection attempt counter for exponential backoff
    while True:
        try:
            print("📡 Connecting to Coinbase ticker (ETH-USD)...")
            async with websockets.connect(COINBASE_WS, ping_interval=20, ping_timeout=10, close_timeout=10) as ws:
                # Subscribe
                sub = {
                    "type": "subscribe",
                    "product_ids": ["ETH-USD"],
                    "channels": ["ticker"],
                }
                await ws.send(json.dumps(sub))
                print("✅ Subscribed to ETH-USD ticker")
                # Reset backoff on successful connection
                attempt = 0

                # Periodic bucket finalizer
                async def bucket_task():
                    nonlocal mean_tick_jump_ema, bucket_start_time, bucket_start_price, bucket_last_price, bucket_count, last_print
                    while True:
                        await asyncio.sleep(BUCKET_MS / 1000.0)
                        now_ms = int(time.time() * 1000)
                        if (
                            bucket_start_time is not None
                            and bucket_start_price is not None
                            and bucket_last_price is not None
                            and (now_ms - bucket_start_time) >= BUCKET_MS
                        ):
                            try:
                                r160 = 100.0 * abs(math.log(bucket_last_price / bucket_start_price))
                            except Exception:
                                r160 = 0.0
                            # Update EMA
                            mean_tick_jump_ema = ALPHA_160MS * r160 + (1 - ALPHA_160MS) * mean_tick_jump_ema
                            bucket_count += 1

                            # Start next bucket from last price
                            bucket_start_time = now_ms
                            bucket_start_price = bucket_last_price

                            # Print periodic status
                            if bucket_count >= MIN_BUCKETS and (time.time() - last_print > 1.0):
                                JL = simple_jump_loss(mean_tick_jump_ema, F_percent)
                                print(
                                    f"JL(simple,no-clamp): μ_J={mean_tick_jump_ema:.6f}%  F={F_percent:.6f}%  JL={JL:.6f}%  buckets={bucket_count}"
                                )
                                last_print = time.time()

                bucket_task_handle = asyncio.create_task(bucket_task())

                try:
                    while True:
                        msg = await asyncio.wait_for(ws.recv(), timeout=30.0)
                        data = json.loads(msg)
                        if data.get("type") != "ticker" or data.get("product_id") != "ETH-USD":
                            continue
                        bid = float(data.get("best_bid", 0) or 0)
                        ask = float(data.get("best_ask", 0) or 0)
                        if bid <= 0 or ask <= 0:
                            continue
                        mid = (bid + ask) / 2.0

                        now_ms = int(time.time() * 1000)
                        if bucket_start_time is None or bucket_start_price is None:
                            bucket_start_time = now_ms
                            bucket_start_price = mid
                            bucket_last_price = mid
                        else:
                            bucket_last_price = mid
                finally:
                    bucket_task_handle.cancel()
                    try:
                        await bucket_task_handle
                    except:
                        pass

        except (asyncio.TimeoutError,
                websockets.exceptions.ConnectionClosed,
                websockets.exceptions.ConnectionClosedOK,
                websockets.exceptions.ConnectionClosedError) as e:
            attempt += 1
            delay = min(60, 2 ** attempt)
            jitter = random.uniform(0.5, 1.5)
            sleep_s = delay * jitter
            print(f"⚠️ Connection issue ({type(e).__name__}), reconnecting in {sleep_s:.1f}s (attempt {attempt})")
            await asyncio.sleep(sleep_s)
        except Exception as e:
            attempt += 1
            delay = min(60, 2 ** attempt)
            jitter = random.uniform(0.5, 1.5)
            sleep_s = delay * jitter
            print(f"⚠️ Coinbase error: {e}, reconnecting in {sleep_s:.1f}s (attempt {attempt})")
            await asyncio.sleep(sleep_s)


def main():
    if len(sys.argv) < 2:
        print("Usage: python3 jumplossPython.py <fee_units>")
        print(" - <fee_units>: thousandths-of-basis-points (e.g., 5000 -> 0.05%)")
        sys.exit(1)
    try:
        fee_units = int(sys.argv[1])
    except ValueError:
        print("Invalid fee_units; must be integer")
        sys.exit(1)

    asyncio.run(run(fee_units))


if __name__ == "__main__":
    main()
