use std::env;
use jumploss_rs::start_background;

#[tokio::main]
async fn main() {
    let args: Vec<String> = env::args().collect();
    if args.len() < 2 {
        eprintln!("Usage: cargo run --release -- <fee_units>\n  <fee_units>: thousandths-of-basis-points (e.g., 5000 -> 0.05%)");
        std::process::exit(1);
    }
    let fee_units: i64 = args[1].parse().unwrap_or_else(|_| {
        eprintln!("Invalid fee_units; must be integer");
        std::process::exit(1);
    });

    // Start background worker and print updates
    let handle = start_background(fee_units);
    let mut rx = handle.updates;
    while let Some(update) = rx.recv().await {
        println!(
            "JL(simple,no-clamp): μ_J={:.6}%  F={:.6}%  JL={:.6}%  buckets={}",
            update.mu_j_percent, update.f_percent, update.jl_percent, update.bucket_count
        );
    }
}
