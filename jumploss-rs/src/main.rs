use std::env;
use std::time::Duration;
use jumploss_rs::start_background;

#[tokio::main]
async fn main() {
    let args: Vec<String> = env::args().collect();
    if args.len() < 2 {
        eprintln!(
            "Usage:\n  cargo run -p jumploss-rs --release -- <fee_units> [--seconds N | --blocks N]\n\n  <fee_units>: thousandths-of-basis-points (e.g., 5000 -> 0.05%)\n  --seconds N: compute dynamic JL with half-life tied to N seconds\n  --blocks N : compute dynamic JL with half-life tied to N blocks (2s per block)\n"
        );
        std::process::exit(1);
    }
    let fee_units: i64 = args[1].parse().unwrap_or_else(|_| {
        eprintln!("Invalid fee_units; must be integer");
        std::process::exit(1);
    });

    // Start background worker
    let handle = start_background(fee_units);

    // Dynamic half-life mode if flags provided
    if args.len() >= 4 {
        let (time_type_init, st_init) = match args[2].as_str() {
            "--seconds" | "-s" => (true, args[3].parse::<u64>().unwrap_or_else(|_| {
                eprintln!("Invalid seconds; must be integer");
                std::process::exit(1);
            })),
            "--blocks" | "-b" => (false, args[3].parse::<u64>().unwrap_or_else(|_| {
                eprintln!("Invalid blocks; must be integer");
                std::process::exit(1);
            })),
            other => {
                eprintln!("Unknown flag: {}\nUse --seconds N or --blocks N", other);
                std::process::exit(1);
            }
        };
        // Live-configurable HL via stdin: lines like "seconds 12" or "blocks 6"
        use tokio::io::{AsyncBufReadExt, BufReader};
        use tokio::sync::watch;
        let (cfg_tx, cfg_rx) = watch::channel((time_type_init, st_init));
        tokio::spawn(async move {
            let stdin = tokio::io::stdin();
            let mut lines = BufReader::new(stdin).lines();
            while let Ok(Some(line)) = lines.next_line().await {
                let parts: Vec<_> = line.trim().split_whitespace().collect();
                if parts.len() == 2 {
                    let key = parts[0].to_ascii_lowercase();
                    if let Ok(v) = parts[1].parse::<u64>() {
                        match key.as_str() {
                            "seconds" | "sec" | "s" => { let _ = cfg_tx.send((true, v)); }
                            "blocks" | "blk" | "b" => { let _ = cfg_tx.send((false, v)); }
                            _ => {}
                        }
                    }
                }
            }
        });

        // Print dynamic JL once per second
        let mut tick = tokio::time::interval(Duration::from_secs(1));
        loop {
            tick.tick().await;
            let (time_type, st) = *cfg_rx.borrow();
            if let Some(upd) = handle.request_jumploss(fee_units, st, time_type).await {
                // Display half-life seconds
                let hl_sec = if time_type { st.saturating_div(2) } else { st };
                println!(
                    "JL(dynamic HL, HL={}s): μ_J={:.6}%  F={:.6}%  JL={:.6}%  buckets={}",
                    hl_sec, upd.mu_j_percent, upd.f_percent, upd.jl_percent, upd.bucket_count
                );
            }
        }
    }

    // Default: print stream with default half-life (4800ms)
    let mut rx = handle.updates;
    while let Some(update) = rx.recv().await {
        println!(
            "JL(simple,no-clamp): μ_J={:.6}%  F={:.6}%  JL={:.6}%  buckets={}",
            update.mu_j_percent, update.f_percent, update.jl_percent, update.bucket_count
        );
    }
}
