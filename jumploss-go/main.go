package main

import (
    "bufio"
    "context"
    "flag"
    "fmt"
    "os"
    "os/signal"
    "strings"
    "syscall"
    "time"

    "jumploss-go/jumploss"
)

func main() {
    usage := func() {
        fmt.Fprintf(os.Stderr, "Usage:\n  %s <fee_units> [--seconds N | --blocks N]\n\n  <fee_units>: thousandths-of-basis-points (e.g., 5000 -> 0.05%%)\n  --seconds N: dynamic HL driven by N seconds (HL = N/2)\n  --blocks N : dynamic HL driven by N blocks (2s/block; HL = blocks)\n", os.Args[0])
    }
    flag.Usage = usage
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

    h, err := jumploss.Start(ctx, feeUnits)
    if err != nil {
        fmt.Println("error:", err)
        os.Exit(1)
    }

    // Dynamic mode?
    if flag.NArg() >= 3 {
        mode := flag.Arg(1)
        var timeTypeSec bool
        var st uint64
        switch mode {
        case "--seconds", "-s":
            timeTypeSec = true
            var v uint64
            if _, err := fmt.Sscanf(flag.Arg(2), "%d", &v); err != nil { fmt.Println("invalid seconds:", err); os.Exit(1) }
            st = v
        case "--blocks", "-b":
            timeTypeSec = false
            var v uint64
            if _, err := fmt.Sscanf(flag.Arg(2), "%d", &v); err != nil { fmt.Println("invalid blocks:", err); os.Exit(1) }
            st = v
        default:
            fmt.Println("unknown flag:", mode)
            os.Exit(1)
        }

        // stdin watcher: lines like "seconds 12" or "blocks 6"
        stType := timeTypeSec
        scanner := bufio.NewScanner(os.Stdin)
        go func() {
            for scanner.Scan() {
                parts := strings.Fields(strings.TrimSpace(scanner.Text()))
                if len(parts) == 2 {
                    switch strings.ToLower(parts[0]) {
                    case "seconds", "sec", "s":
                        stType = true
                        var nv uint64
                        if _, err := fmt.Sscanf(parts[1], "%d", &nv); err == nil { st = nv }
                    case "blocks", "blk", "b":
                        stType = false
                        var nv uint64
                        if _, err := fmt.Sscanf(parts[1], "%d", &nv); err == nil { st = nv }
                    }
                }
            }
        }()

        // print dynamic JL once per second
        ticker := time.NewTicker(1 * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                resp := make(chan jumploss.Update, 1)
                // send query or exit on cancel
                select {
                case h.Query <- jumploss.Query{ FeeUnits: feeUnits, SettlementTime: st, TimeTypeSec: stType, Resp: resp }:
                case <-ctx.Done():
                    return
                }
                // wait for response, cancel, or timeout
                var upd jumploss.Update
                select {
                case upd = <-resp:
                case <-ctx.Done():
                    return
                case <-time.After(2 * time.Second):
                    // skip this tick if no response
                    continue
                }
                var hlSec uint64
                if stType { hlSec = st / 2 } else { hlSec = st }
                fmt.Printf("JL(dynamic HL, HL=%ds): μ_J=%.6f%%  F=%.6f%%  JL=%.6f%%  buckets=%d\n",
                    hlSec, upd.MuJPercent, upd.FPercent, upd.JLPercent, upd.BucketCount)
            }
        }
    }

    // default stream
    for u := range h.Updates {
        fmt.Printf("JL(simple,no-clamp): μ_J=%.6f%%  F=%.6f%%  JL=%.6f%%  buckets=%d\n",
            u.MuJPercent, u.FPercent, u.JLPercent, u.BucketCount)
    }
}
