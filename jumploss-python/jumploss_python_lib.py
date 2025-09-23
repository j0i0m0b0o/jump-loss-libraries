#!/usr/bin/env python3
"""
Jump Loss background worker (Python)

Library-style API that spawns its own background worker and does not
interfere with the caller's event loop or threads. It connects to the
Coinbase ETH-USD ticker, computes 160ms bucket EMA (μ_J, percent), and
publishes simple jump loss (no clamp/cap) via callback and/or thread-safe queue.

Usage (from other code):

    from jumploss_python_lib import JumpLossWorker

    def on_update(u):
        print('JL update:', u)

    jl = JumpLossWorker(fee_units=5000, on_update=on_update)
    jl.start_background()
    # ... later ...
    jl.stop()

The worker owns its own asyncio loop in a dedicated thread.
"""
from __future__ import annotations

import asyncio
import json
import math
import random
import threading
import time
from dataclasses import dataclass
from typing import Callable, Optional

import websockets
import websockets.exceptions
import queue as thread_queue

COINBASE_WS = "wss://ws-feed.exchange.coinbase.com"
# Defaults: 160ms buckets, ~4.8s half-life => alpha ≈ 0.0231
DEFAULT_BUCKET_MS = 160
DEFAULT_HALF_LIFE_MS = 4800
MIN_BUCKETS = 5


def units_to_percent(units: int) -> float:
    return (units / 1e7) * 100.0


def simple_jump_loss(mu_j_percent: float, f_percent: float) -> float:
    return mu_j_percent - (f_percent / 2.0) if mu_j_percent > f_percent else mu_j_percent / 2.0


@dataclass
class JumpLossUpdate:
    timestamp: float
    mu_j_percent: float
    jl_percent: float
    f_percent: float
    bucket_count: int


class JumpLossWorker:
    def __init__(self, fee_units: int, on_update: Optional[Callable[[JumpLossUpdate], None]] = None,
                 bucket_ms: int = DEFAULT_BUCKET_MS, half_life_ms: int = DEFAULT_HALF_LIFE_MS) -> None:
        self.fee_units = int(fee_units)
        self.f_percent = units_to_percent(self.fee_units)
        self.on_update = on_update
        self.bucket_ms = max(1, int(bucket_ms))
        self.half_life_ms = max(1, int(half_life_ms))
        # Compute alpha from half-life: alpha = 1 - 2^(-Δ/T_half)
        self._alpha = 1.0 - pow(2.0, -float(self.bucket_ms) / float(self.half_life_ms))
        self._thread: Optional[threading.Thread] = None
        self._stop_evt = threading.Event()
        self.updates = thread_queue.Queue(maxsize=1024)  # thread-safe for consumers

        # mutable state for worker
        self._mean_tick_jump_ema = 0.0
        self._bucket_start_price: Optional[float] = None
        self._bucket_last_price: Optional[float] = None
        self._bucket_count = 0
        self._last_print = 0.0

    def start_background(self) -> None:
        if self._thread and self._thread.is_alive():
            return
        self._stop_evt.clear()
        self._thread = threading.Thread(target=self._thread_main, name="JumpLossWorker", daemon=True)
        self._thread.start()

    def stop(self, timeout: Optional[float] = 5.0) -> None:
        self._stop_evt.set()
        if self._thread and self._thread.is_alive():
            self._thread.join(timeout)

    def _thread_main(self) -> None:
        try:
            asyncio.run(self._run_loop())
        except Exception as e:
            # Swallow exceptions to avoid killing caller
            print(f"JumpLossWorker exited with error: {e}")

    async def _run_loop(self) -> None:
        attempt = 0
        while not self._stop_evt.is_set():
            try:
                async with websockets.connect(COINBASE_WS, ping_interval=20, ping_timeout=10, close_timeout=10) as ws:
                    sub = {"type": "subscribe", "product_ids": ["ETH-USD"], "channels": ["ticker"]}
                    await ws.send(json.dumps(sub))
                    attempt = 0  # reset backoff

                    # spawn bucket finalizer
                    bucket_task = asyncio.create_task(self._bucket_finalizer())
                    try:
                        while not self._stop_evt.is_set():
                            msg = await asyncio.wait_for(ws.recv(), timeout=30.0)
                            if not isinstance(msg, (bytes, str)):
                                continue
                            data = json.loads(msg)
                            if data.get("type") != "ticker" or data.get("product_id") != "ETH-USD":
                                continue
                            try:
                                bid = float(data.get("best_bid") or 0)
                                ask = float(data.get("best_ask") or 0)
                            except Exception:
                                continue
                            if bid <= 0 or ask <= 0:
                                continue
                            mid = (bid + ask) / 2.0
                            if self._bucket_start_price is None or self._bucket_last_price is None:
                                self._bucket_start_price = mid
                                self._bucket_last_price = mid
                            else:
                                self._bucket_last_price = mid
                    finally:
                        bucket_task.cancel()
                        with contextlib_suppress():
                            await bucket_task
            except (asyncio.TimeoutError,
                    websockets.exceptions.ConnectionClosed,
                    websockets.exceptions.ConnectionClosedOK,
                    websockets.exceptions.ConnectionClosedError) as e:
                attempt += 1
                delay = min(60, 2 ** attempt)
                jitter = random.uniform(0.5, 1.5)
                sleep_s = delay * jitter
                await asyncio.sleep(sleep_s)
            except Exception:
                attempt += 1
                delay = min(60, 2 ** attempt)
                jitter = random.uniform(0.5, 1.5)
                sleep_s = delay * jitter
                await asyncio.sleep(sleep_s)

    async def _bucket_finalizer(self) -> None:
        while not self._stop_evt.is_set():
            await asyncio.sleep(self.bucket_ms / 1000.0)
            if self._bucket_start_price is None or self._bucket_last_price is None:
                continue
            start = self._bucket_start_price
            last = self._bucket_last_price
            try:
                r160 = 100.0 * abs(math.log(last / start))
            except Exception:
                r160 = 0.0
            self._mean_tick_jump_ema = self._alpha * r160 + (1 - self._alpha) * self._mean_tick_jump_ema
            self._bucket_count += 1
            # start next bucket from last
            self._bucket_start_price = last

            # publish update (>= min buckets, at most 1Hz)
            now = time.time()
            if self._bucket_count >= MIN_BUCKETS and (now - self._last_print) > 1.0:
                jl = simple_jump_loss(self._mean_tick_jump_ema, self.f_percent)
                update = JumpLossUpdate(
                    timestamp=now,
                    mu_j_percent=self._mean_tick_jump_ema,
                    jl_percent=jl,
                    f_percent=self.f_percent,
                    bucket_count=self._bucket_count,
                )
                try:
                    if self.on_update:
                        self.on_update(update)
                    self.updates.put_nowait(update)
                except Exception:
                    pass
                self._last_print = now


class contextlib_suppress:
    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        return True
