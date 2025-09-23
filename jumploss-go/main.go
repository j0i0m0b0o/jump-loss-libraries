package main

import (
    "context"
    "flag"
    "fmt"
    "os"
    "os/signal"
    "syscall"

    "jumploss-go/jumploss"
)

func main() {
    flag.Usage = func() {
        fmt.Fprintf(os.Stderr, "Usage: %s <fee_units>\n  <fee_units>: thousandths-of-basis-points (e.g., 5000 -> 0.05%%)\n", os.Args[0])
    }
    flag.Parse()
    if flag.NArg() < 1 {
        flag.Usage()
        os.Exit(1)
    }
    var feeUnits int64
    if _, err := fmt.Sscanf(flag.Arg(0), "%d", &feeUnits); err != nil {
        fmt.Println("invalid fee_units:", err)
        os.Exit(1)
    }

    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    updates, err := jumploss.Start(ctx, feeUnits)
    if err != nil {
        fmt.Println("error:", err)
        os.Exit(1)
    }

    for u := range updates {
        fmt.Printf("JL(simple,no-clamp): μ_J=%.6f%%  F=%.6f%%  JL=%.6f%%  buckets=%d\n",
            u.MuJPercent, u.FPercent, u.JLPercent, u.BucketCount)
    }
}

