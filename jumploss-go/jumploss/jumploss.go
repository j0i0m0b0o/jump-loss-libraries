package jumploss

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "math"
    mrand "math/rand"
    "sync"
    "time"

    "github.com/gobwas/ws"
    "github.com/gobwas/ws/wsutil"
)

const (
    coinbaseWS = "wss://ws-feed.exchange.coinbase.com"
    defaultBucketMS   = 160
    defaultHalfLifeMS = 4800 // ~4.8s => alpha ≈ 0.0231
    minBuckets = 5
    maxBackoff = 60 * time.Second
)

type Update struct {
    Time        time.Time
    MuJPercent  float64
    JLPercent   float64
    FPercent    float64
    BucketCount int64
}

// Query request for dynamic half-life computation
type Query struct {
    FeeUnits       int64
    SettlementTime uint64
    TimeTypeSec    bool // true=seconds; false=blocks (2s/block)
    Resp           chan Update
}

type state struct {
    muJPercent       float64
    bucketStartPrice *float64
    bucketLastPrice  *float64
    bucketCount      int64
    mu               sync.Mutex

    // ring buffer of recent 160ms returns (timestamp ms, r%)
    rets      []retEntry
    firstIdx  int
}

type retEntry struct {
    tsMs int64
    rPct float64
}

func unitsToPercent(units int64) float64 { return (float64(units) / 1e7) * 100.0 }
func simpleJumpLoss(muJ, F float64) float64 {
    if muJ > F { return muJ - (F / 2.0) }
    return muJ / 2.0
}

type subscribeMsg struct {
    Type       string   `json:"type"`
    ProductIDs []string `json:"product_ids"`
    Channels   []string `json:"channels"`
}
type tickerMsg struct {
    Type      string `json:"type"`
    ProductID string `json:"product_id"`
    BestBid   string `json:"best_bid"`
    BestAsk   string `json:"best_ask"`
}

type Config struct {
    BucketMS   int
    HalfLifeMS int
}

// Handle exposes updates stream and a query channel for dynamic half-life JL.
type Handle struct {
    Updates <-chan Update
    Query   chan Query
}

// Start launches a self-contained worker with default config.
func Start(ctx context.Context, feeUnits int64) (*Handle, error) {
    return StartWithConfig(ctx, feeUnits, Config{BucketMS: defaultBucketMS, HalfLifeMS: defaultHalfLifeMS})
}

// StartWithConfig launches with explicit config (bucket length and half-life in ms).
func StartWithConfig(ctx context.Context, feeUnits int64, cfg Config) (*Handle, error) {
    F := unitsToPercent(feeUnits)
    out := make(chan Update, 1024)
    qch := make(chan Query, 256)
    mrand.Seed(time.Now().UnixNano())
    st := &state{}
    if cfg.BucketMS <= 0 { cfg.BucketMS = defaultBucketMS }
    if cfg.HalfLifeMS <= 0 { cfg.HalfLifeMS = defaultHalfLifeMS }
    alpha := 1.0 - math.Pow(2.0, -float64(cfg.BucketMS)/float64(cfg.HalfLifeMS))

    go func() {
        defer close(out)
        attempt := 0
        // query server
        go func() {
            for {
                select {
                case <-ctx.Done():
                    return
                case q := <-qch:
                    // half-life = settlementTime/2; if blocks, 2s/block -> HL seconds = blocks
                    var hlMs int64
                    if q.TimeTypeSec {
                        hlMs = int64(q.SettlementTime)*1000/2
                    } else {
                        hlMs = int64(q.SettlementTime)*2000/2
                    }
                    mu := computeDynamicMuJ(st, cfg.BucketMS, hlMs)
                    jl := simpleJumpLoss(mu, unitsToPercent(q.FeeUnits))
                    var bc int64
                    st.mu.Lock(); bc = st.bucketCount; st.mu.Unlock()
                    q.Resp <- Update{ Time: time.Now(), MuJPercent: mu, JLPercent: jl, FPercent: unitsToPercent(q.FeeUnits), BucketCount: bc }
                }
            }
        }()
        for {
            select {
            case <-ctx.Done():
                return
            default:
            }
            conn, _, _, err := ws.Dial(ctx, coinbaseWS)
            if err != nil {
                attempt++
                backoffSleep(attempt)
                continue
            }
            attempt = 0

            // Subscribe
            sub := subscribeMsg{Type: "subscribe", ProductIDs: []string{"ETH-USD"}, Channels: []string{"ticker"}}
            payload, _ := json.Marshal(sub)
            if err := wsutil.WriteClientMessage(conn, ws.OpText, payload); err != nil { _ = conn.Close(); attempt++; backoffSleep(attempt); continue }
            connCtx, cancel := context.WithCancel(ctx)
            go bucketLoop(connCtx, st, F, out, cfg, alpha)

            for {
                select {
                case <-ctx.Done():
                    cancel()
                    _ = conn.Close()
                    return
                default:
                }
                // Read next server message
                hdr, r, err := wsutil.NextReader(conn, ws.StateClientSide)
                if err != nil { cancel(); _ = conn.Close(); attempt++; backoffSleep(attempt); break }
                switch hdr.OpCode {
                case ws.OpText:
                    data, _ := io.ReadAll(r)
                    var base map[string]any
                    if err := json.Unmarshal(data, &base); err != nil { continue }
                    if t, _ := base["type"].(string); t != "ticker" { continue }
                    var tm tickerMsg
                    if err := json.Unmarshal(data, &tm); err != nil { continue }
                    if tm.ProductID != "ETH-USD" { continue }
                    var bid, ask float64
                    if _, err := fmt.Sscanf(tm.BestBid, "%f", &bid); err != nil || bid <= 0 { continue }
                    if _, err := fmt.Sscanf(tm.BestAsk, "%f", &ask); err != nil || ask <= 0 { continue }
                    mid := (bid + ask) / 2.0
                    st.mu.Lock()
                    if st.bucketStartPrice == nil || st.bucketLastPrice == nil { st.bucketStartPrice = &mid; st.bucketLastPrice = &mid } else { st.bucketLastPrice = &mid }
                    st.mu.Unlock()
                case ws.OpPing:
                    _ = wsutil.WriteClientMessage(conn, ws.OpPong, nil)
                case ws.OpClose:
                    cancel(); _ = conn.Close(); attempt++; backoffSleep(attempt); goto next
                default:
                    // ignore
                }
            }
        next:
        }
    }()

    return &Handle{Updates: out, Query: qch}, nil
}

func bucketLoop(ctx context.Context, st *state, F float64, out chan<- Update, cfg Config, alpha float64) {
    t := time.NewTicker(time.Duration(cfg.BucketMS) * time.Millisecond)
    defer t.Stop()
    var lastEmit time.Time
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
        }
        st.mu.Lock()
        var emit bool
        var upd Update
        if st.bucketStartPrice != nil && st.bucketLastPrice != nil {
            start, last := *st.bucketStartPrice, *st.bucketLastPrice
            r := math.Abs(math.Log(last/start)) * 100.0
            st.muJPercent = alpha*r + (1.0-alpha)*st.muJPercent
            st.bucketCount++
            st.bucketStartPrice = &last
            // append to returns and prune to ~10 minutes
            ts := time.Now().UnixMilli()
            st.rets = append(st.rets, retEntry{tsMs: ts, rPct: r})
            cutoff := ts - 600_000
            for st.firstIdx < len(st.rets) {
                if st.rets[st.firstIdx].tsMs < cutoff { st.firstIdx++ } else { break }
            }
            if st.firstIdx > 1024 && st.firstIdx > len(st.rets)/2 {
                // compact slice
                st.rets = append([]retEntry(nil), st.rets[st.firstIdx:]...)
                st.firstIdx = 0
            }
            if st.bucketCount >= minBuckets && (lastEmit.IsZero() || time.Since(lastEmit) >= time.Second) {
                jl := simpleJumpLoss(st.muJPercent, F)
                upd = Update{ Time: time.Now(), MuJPercent: st.muJPercent, JLPercent: jl, FPercent: F, BucketCount: st.bucketCount }
                emit = true
                lastEmit = time.Now()
            }
        }
        st.mu.Unlock()
        if emit {
            select {
            case out <- upd:
            default:
                // drop if no consumer to avoid blocking
            }
        }
    }
}

func backoffSleep(attempt int) {
    base := time.Duration(math.Pow(2, float64(min(attempt, 6)))) * time.Second
    if base > maxBackoff { base = maxBackoff }
    jitter := 0.5 + mrand.Float64()
    sleep := time.Duration(float64(base) * jitter)
    if sleep > maxBackoff { sleep = maxBackoff }
    time.Sleep(sleep)
}

func min(a, b int) int { if a < b { return a }; return b }

// computeDynamicMuJ computes μ_J for a given half-life from the stored returns buffer.
func computeDynamicMuJ(st *state, bucketMS int, hlMs int64) float64 {
    if hlMs <= 0 { hlMs = 1 }
    alpha := 1.0 - math.Pow(2.0, -float64(bucketMS)/float64(hlMs))
    // copy returns under lock
    st.mu.Lock()
    rets := make([]retEntry, len(st.rets)-st.firstIdx)
    copy(rets, st.rets[st.firstIdx:])
    st.mu.Unlock()
    acc := 0.0
    for _, e := range rets { acc = alpha*e.rPct + (1.0-alpha)*acc }
    return acc
}
