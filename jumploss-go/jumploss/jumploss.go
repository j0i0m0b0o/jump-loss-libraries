package jumploss

import (
    "context"
    "encoding/json"
    "fmt"
    "math"
    mrand "math/rand"
    "net/http"
    "sync"
    "time"

    "github.com/gorilla/websocket"
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

type state struct {
    muJPercent       float64
    bucketStartPrice *float64
    bucketLastPrice  *float64
    bucketCount      int64
    mu               sync.Mutex
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

// Start launches a self-contained worker with default config.
func Start(ctx context.Context, feeUnits int64) (<-chan Update, error) {
    return StartWithConfig(ctx, feeUnits, Config{BucketMS: defaultBucketMS, HalfLifeMS: defaultHalfLifeMS})
}

// StartWithConfig launches with explicit config (bucket length and half-life in ms).
func StartWithConfig(ctx context.Context, feeUnits int64, cfg Config) (<-chan Update, error) {
    F := unitsToPercent(feeUnits)
    out := make(chan Update, 1024)
    mrand.Seed(time.Now().UnixNano())
    st := &state{}
    if cfg.BucketMS <= 0 { cfg.BucketMS = defaultBucketMS }
    if cfg.HalfLifeMS <= 0 { cfg.HalfLifeMS = defaultHalfLifeMS }
    alpha := 1.0 - math.Pow(2.0, -float64(cfg.BucketMS)/float64(cfg.HalfLifeMS))

    go func() {
        defer close(out)
        attempt := 0
        for {
            select {
            case <-ctx.Done():
                return
            default:
            }
            d := websocket.Dialer{HandshakeTimeout: 15 * time.Second, EnableCompression: true, Proxy: http.ProxyFromEnvironment}
            conn, _, err := d.DialContext(ctx, coinbaseWS, nil)
            if err != nil {
                attempt++
                backoffSleep(attempt)
                continue
            }
            attempt = 0

            // Subscribe
            sub := subscribeMsg{Type: "subscribe", ProductIDs: []string{"ETH-USD"}, Channels: []string{"ticker"}}
            if err := conn.WriteJSON(sub); err != nil { _ = conn.Close(); attempt++; backoffSleep(attempt); continue }

            connCtx, cancel := context.WithCancel(ctx)
            go bucketLoop(connCtx, st, F, out, cfg, alpha)
            _ = conn.SetReadDeadline(time.Now().Add(35 * time.Second))
            conn.SetPongHandler(func(string) error { _ = conn.SetReadDeadline(time.Now().Add(35 * time.Second)); return nil })

            for {
                select {
                case <-ctx.Done():
                    cancel()
                    _ = conn.Close()
                    return
                default:
                }
                _, data, err := conn.ReadMessage()
                if err != nil { cancel(); _ = conn.Close(); attempt++; backoffSleep(attempt); break }
                _ = conn.SetReadDeadline(time.Now().Add(35 * time.Second))
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
            }
        }
    }()

    return out, nil
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
        if st.bucketStartPrice != nil && st.bucketLastPrice != nil {
            start, last := *st.bucketStartPrice, *st.bucketLastPrice
            r := math.Abs(math.Log(last/start)) * 100.0
            st.muJPercent = alpha*r + (1.0-alpha)*st.muJPercent
            st.bucketCount++
            st.bucketStartPrice = &last
            if st.bucketCount >= minBuckets && (lastEmit.IsZero() || time.Since(lastEmit) >= time.Second) {
                jl := simpleJumpLoss(st.muJPercent, F)
                out <- Update{ Time: time.Now(), MuJPercent: st.muJPercent, JLPercent: jl, FPercent: F, BucketCount: st.bucketCount }
                lastEmit = time.Now()
            }
        }
        st.mu.Unlock()
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
