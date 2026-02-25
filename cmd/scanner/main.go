package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"ocdex/config"
	"ocdex/internal/cexstream"
	"ocdex/internal/discovery"
	"ocdex/internal/engine"
	"ocdex/internal/registry"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/olekukonko/tablewriter"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

var (
	configPath = flag.String("config", "config", "配置文件目录")
	noGui      = flag.Bool("no-gui", false, "禁止终端表格显示")
)

func main() {
	flag.Parse()

	if !*noGui {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}).Level(zerolog.InfoLevel)
	} else {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "2006-01-02T15:04:05+08:00"})
	}

	log.Info().Msg("CDEX 套利扫描器启动中...")

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("加载配置失败")
	}

	ethClient, err := ethclient.Dial(cfg.Chain.BSC.RPC)
	if err != nil {
		log.Fatal().Err(err).Msg("连接 BSC 节点失败")
	}
	defer ethClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Discover tokens
	disc := discovery.NewBinanceDiscovery(cfg.Exchange.Binance, ethClient)
	tokens, err := disc.DiscoverBSCTokens(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("代币发现失败")
	}

	maxTokens := cfg.Scanner.MaxTokens
	if maxTokens > 0 && len(tokens) > maxTokens {
		tokens = tokens[:maxTokens]
	}

	// Registry
	reg := registry.NewTokenRegistry()
	cexSymbols := make([]string, 0, len(tokens))
	for _, t := range tokens {
		reg.RegisterToken(t)
		cexSymbols = append(cexSymbols, t.CEXSymbol)
	}

	// Discover pools
	pools := disc.DiscoverPools(ctx, tokens, cfg.Chain.BSC)

	// Pool manager
	taxDetector := engine.NewTaxDetector(ethClient, cfg.Chain.BSC)
	poolManager := engine.NewPoolManager(taxDetector)
	for symbol, pool := range pools {
		token, ok := reg.GetToken(symbol)
		if !ok {
			continue
		}
		poolManager.AddPool(pool.PairAddress, pool.Token0, pool.Token1, symbol, token.ContractAddress, token.Decimals)
	}

	// Discover and register V3 pools
	if cfg.Chain.BSC.FactoryV3 != "" {
		v3pools := disc.DiscoverPoolsV3(ctx, tokens, cfg.Chain.BSC)
		for symbol, pool := range v3pools {
			token, ok := reg.GetToken(symbol)
			if !ok {
				continue
			}
			poolManager.AddPoolV3(pool.PairAddress, pool.Token0, pool.Token1, symbol, token.ContractAddress, token.Decimals, pool.FeeTier)
		}
		log.Info().Int("v3_pools", len(v3pools)).Msg("V3 池注册完成")
	}

	// CEX stream
	cexStream := cexstream.NewStream()
	cexStream.SetSymbols(cexSymbols)
	go cexStream.Start(ctx)

	// LogWatcher for DEX price updates
	wsURL := cfg.Chain.BSC.WsURL
	if wsURL == "" {
		wsURL = strings.Replace(cfg.Chain.BSC.RPC, "https://", "wss://", 1)
	}
	logWatcher := engine.NewLogWatcher(wsURL, poolManager, nil)
	go logWatcher.Start(ctx)

	log.Info().Int("tokens", len(tokens)).Int("pools", len(pools)).Msg("扫描器已启动")

	// Dashboard refresh loop
	if !*noGui {
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					renderDashboard(reg, poolManager, cexStream.SpotCache)
				}
			}
		}()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	cancel()
	log.Info().Msg("扫描器已停止")
}

func renderDashboard(reg *registry.TokenRegistry, pm *engine.PoolManager, cache *cexstream.PriceCache) {
	tokens := reg.GetAllTokens()

	type row struct {
		Symbol   string
		CEX      decimal.Decimal
		DEX      decimal.Decimal
		Spread   decimal.Decimal
		HasData  bool
	}

	var rows []row
	for _, t := range tokens {
		cex, cexOk := cache.Get(t.Symbol)
		dex := pm.GetPrice(t.ContractAddress, t.Decimals)

		r := row{Symbol: t.Symbol, CEX: cex, DEX: dex}
		if cexOk && !cex.IsZero() && !dex.IsZero() {
			r.Spread = cex.Sub(dex).Div(dex).Mul(decimal.NewFromInt(100))
			r.HasData = true
		}
		rows = append(rows, r)
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Spread.Abs().GreaterThan(rows[j].Spread.Abs())
	})

	fmt.Print("\033[H\033[2J")
	fmt.Printf("CDEX 扫描器 | 币种: %d | %s\n\n", len(rows), time.Now().Format("15:04:05"))

	table := tablewriter.NewWriter(os.Stdout)
	table.Header("Symbol", "CEX", "DEX", "Spread", "Status")

	for i, r := range rows {
		if i >= 20 {
			break
		}
		status := "OK"
		if !r.HasData {
			status = "等待数据"
		}
		spreadStr := r.Spread.StringFixed(3) + "%"
		table.Append(r.Symbol, r.CEX.StringFixed(4), r.DEX.StringFixed(4), spreadStr, status)
	}

	table.Render()
	fmt.Println("\n按 Ctrl+C 退出")
}
