use futures_util::{SinkExt, StreamExt};
use rand::Rng;
use serde::Serialize;
use serde_json::json;
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tokio::runtime::Builder;
use tokio::sync::{mpsc, watch};
use tokio::time::{sleep, timeout};
use tokio_tungstenite::connect_async;
use tungstenite::Message;

const COINBASE_WS: &str = "wss://ws-feed.exchange.coinbase.com";
const DEFAULT_BUCKET_MS: u64 = 160;
const DEFAULT_HALF_LIFE_MS: u64 = 4800; // ~4.8s => alpha ≈ 0.0231
const MIN_BUCKETS: u64 = 5;

#[derive(Default)]
struct State {
    mean_tick_jump_ema: f64,            // μ_J in percent
    bucket_start_price: Option<f64>,
    bucket_last_price: Option<f64>,
    bucket_count: u64,
    last_emit: Option<Instant>,
}

#[derive(Debug, Clone, Serialize)]
pub struct JumpLossUpdate {
    pub timestamp_ms: u128,
    pub mu_j_percent: f64,
    pub jl_percent: f64,
    pub f_percent: f64,
    pub bucket_count: u64,
}

pub struct JumpLossHandle {
    pub updates: mpsc::Receiver<JumpLossUpdate>,
    pub shutdown: watch::Sender<bool>,
    _join_handle: thread::JoinHandle<()>,
}

fn units_to_percent(units: i64) -> f64 {
    (units as f64 / 1e7_f64) * 100.0
}

fn simple_jump_loss(mu_j_percent: f64, f_percent: f64) -> f64 {
    if mu_j_percent > f_percent {
        mu_j_percent - (f_percent / 2.0)
    } else {
        mu_j_percent / 2.0
    }
}

pub struct Config {
    pub bucket_ms: u64,
    pub half_life_ms: u64,
}

impl Default for Config {
    fn default() -> Self {
        Self { bucket_ms: DEFAULT_BUCKET_MS, half_life_ms: DEFAULT_HALF_LIFE_MS }
    }
}

pub fn start_background(fee_units: i64) -> JumpLossHandle {
    start_with_config(fee_units, Config::default())
}

pub fn start_with_config(fee_units: i64, cfg: Config) -> JumpLossHandle {
    let (tx, rx) = mpsc::channel::<JumpLossUpdate>(1024);
    let (shutdown_tx, shutdown_rx) = watch::channel(false);

    let join = thread::spawn(move || {
        let rt = Builder::new_multi_thread()
            .enable_all()
            .worker_threads(2)
            .thread_name("jumploss-rs")
            .build()
            .expect("build rt");
        rt.block_on(run_inner(fee_units, cfg, tx, shutdown_rx));
    });

    JumpLossHandle { updates: rx, shutdown: shutdown_tx, _join_handle: join }
}

async fn run_inner(
    fee_units: i64,
    cfg: Config,
    updates: mpsc::Sender<JumpLossUpdate>,
    shutdown: watch::Receiver<bool>,
) {
    let f_percent = units_to_percent(fee_units);
    let bucket_ms = cfg.bucket_ms.max(1);
    let half_life_ms = cfg.half_life_ms.max(1);
    let alpha = 1.0 - 2_f64.powf(-(bucket_ms as f64) / (half_life_ms as f64));
    let state = Arc::new(Mutex::new(State::default()));
    let mut attempt: u32 = 0;

    loop {
        if *shutdown.borrow() { break; }
        match connect_async(COINBASE_WS).await {
            Ok((mut ws, _)) => {
                let sub = json!({
                    "type": "subscribe",
                    "product_ids": ["ETH-USD"],
                    "channels": ["ticker"],
                });
                if ws.send(Message::Text(sub.to_string())).await.is_err() {
                    // fall through to backoff
                } else {
                    attempt = 0;
                }

                // bucket task
                let state_for_bucket = state.clone();
                let mut shutdown_for_bucket = shutdown.clone();
                let mut interval = tokio::time::interval(Duration::from_millis(bucket_ms));
                let bucket_task = tokio::spawn(async move {
                    loop {
                        tokio::select! {
                            _ = interval.tick() => {
                                let mut st = state_for_bucket.lock().unwrap();
                                if let (Some(start), Some(last)) = (st.bucket_start_price, st.bucket_last_price) {
                                    let r160 = (last / start).ln().abs() * 100.0;
                                    st.mean_tick_jump_ema = alpha * r160 + (1.0 - alpha) * st.mean_tick_jump_ema;
                                    st.bucket_count += 1;
                                    st.bucket_start_price = Some(last);
                                }
                            }
                            _ = shutdown_for_bucket.changed() => {
                                if *shutdown_for_bucket.borrow() { break; }
                            }
                        }
                    }
                });

                // read loop
                loop {
                    if *shutdown.borrow() { break; }
                    match timeout(Duration::from_secs(30), ws.next()).await {
                        Ok(Some(Ok(msg))) => {
                            if msg.is_text() {
                                if let Err(_) = handle_message(msg.into_text().unwrap(), &state) {}
                                // Emit update at most 1Hz after min buckets
                                let mut st = state.lock().unwrap();
                                let should_emit = st.bucket_count >= MIN_BUCKETS && st
                                    .last_emit
                                    .map(|t| t.elapsed() >= Duration::from_secs(1))
                                    .unwrap_or(true);
                                if should_emit {
                                    let mu = st.mean_tick_jump_ema;
                                    let jl = simple_jump_loss(mu, f_percent);
                                    let ts = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_millis();
                                    let _ = updates.try_send(JumpLossUpdate { timestamp_ms: ts, mu_j_percent: mu, jl_percent: jl, f_percent, bucket_count: st.bucket_count });
                                    st.last_emit = Some(Instant::now());
                                }
                            } else if msg.is_close() {
                                break;
                            }
                        }
                        Ok(Some(Err(_))) => { break; }
                        Ok(None) => { break; }
                        Err(_) => { break; }
                    }
                }

                // stop bucket task
                let _ = bucket_task.abort();
            }
            Err(_) => {}
        }

        if *shutdown.borrow() { break; }
        attempt = attempt.saturating_add(1);
        let base = 2_u64.saturating_pow(attempt.min(6));
        let delay = base.min(60);
        let jitter: f64 = rand::thread_rng().gen_range(0.5..1.5);
        let sleep_ms = ((delay as f64 * jitter).min(60.0) * 1000.0) as u64;
        sleep(Duration::from_millis(sleep_ms)).await;
    }
}

fn handle_message(txt: String, state: &Arc<Mutex<State>>) -> Result<(), Box<dyn std::error::Error>> {
    let v: serde_json::Value = serde_json::from_str(&txt)?;
    if v.get("type").and_then(|t| t.as_str()) == Some("ticker")
        && v.get("product_id").and_then(|p| p.as_str()) == Some("ETH-USD")
    {
        let bid = v.get("best_bid").and_then(|b| b.as_str()).and_then(|s| s.parse::<f64>().ok()).unwrap_or(0.0);
        let ask = v.get("best_ask").and_then(|b| b.as_str()).and_then(|s| s.parse::<f64>().ok()).unwrap_or(0.0);
        if bid > 0.0 && ask > 0.0 {
            let mid = (bid + ask) / 2.0;
            let mut st = state.lock().unwrap();
            if st.bucket_start_price.is_none() {
                st.bucket_start_price = Some(mid);
                st.bucket_last_price = Some(mid);
            } else {
                st.bucket_last_price = Some(mid);
            }
        }
    }
    Ok(())
}
