package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"ocdex/config"
	"ocdex/external/cex"
	"ocdex/internal/capital"
	"ocdex/internal/cexstream"
	"ocdex/internal/discovery"
	"ocdex/internal/engine"
	"ocdex/internal/execution"
	"ocdex/internal/registry"
	"ocdex/internal/storage"
	"ocdex/pkg/logger"
	"ocdex/pkg/notify"
	"ocdex/pkg/wallet"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
)

var liveMode = flag.Bool("live", false, "启用实盘交易模式 (默认为模拟模式)")

func main() {
	flag.Parse()

	// 1. 加载配置
	cfg, err := config.LoadConfig("config")
	if err != nil {
		panic("加载配置失败: " + err.Error())
	}

	// 2. 初始化日志
	logger.InitLogger(cfg.Log.Level)
	log.Info().Msg("CDEX 套利机器人启动中 (事件驱动模式)...")

	// 3. 基础组件
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 3.1 BSC 链客户端 (HTTP, for transactions)
	ethClient, err := ethclient.Dial(cfg.Chain.BSC.RPC)
	if err != nil {
		log.Fatal().Err(err).Msg("无法连接 BSC 节点")
	}

	// 3.2 数据库 (可选)
	var db *storage.MySQL
	if cfg.Database.Enabled {
		db, err = storage.NewMySQL(cfg.Database)
		if err != nil {
			log.Warn().Err(err).Msg("数据库连接失败，将不保存记录")
		}
	}

	// 3.3 通知器
	notifier := notify.NewMultiNotifier(cfg.Notify)

	// 3.4 钱包
	wm, err := wallet.NewWalletManager(cfg.Wallet.Mnemonic)
	if err != nil {
		log.Fatal().Err(err).Msg("钱包初始化失败")
	}
	log.Info().
		Str("钱包地址", wm.GetAddress()).
		Str("Router", cfg.Chain.BSC.RouterV2).
		Str("USDT", cfg.Chain.BSC.USDT).
		Str("WBNB", cfg.Chain.BSC.WBNB).
		Msg("钱包已就绪，请确保钱包有足够 BNB(gas) 和 USDT")

	// 4. CEX 客户端
	binClient := cex.NewBinanceClient(cfg.Exchange.Binance)
	binFuturesClient := cex.NewBinanceFuturesClient(cfg.Exchange.Binance)

	// 4.1 Redis 资金管理
	capitalMgr := capital.NewRedisCapitalManager(*cfg)
	if err := capitalMgr.Init(ctx); err != nil {
		log.Warn().Err(err).Msg("Redis 初始化失败，资金管理可能不可用")
	}

	// 4.2 合约权限检查
	if cfg.Execution.Mode == config.ModeFuturesHedge {
		if err := binFuturesClient.CheckFuturesPermission(); err != nil {
			log.Fatal().Err(err).Msg("合约权限检查未通过")
		}
		log.Info().Msg("合约权限检查通过")
	}

	// 5. 执行器
	var mainExec execution.Executor
	if *liveMode {
		log.Warn().Msg("========= 实盘交易模式 =========")
		realExec, err := execution.NewRealExecutor(
			*cfg, notifier, db, ethClient,
			binClient, binFuturesClient, capitalMgr,
			wm.PrivateKey, wm.Address,
		)
		if err != nil {
			log.Fatal().Err(err).Msg("初始化实盘执行器失败")
		}
		mainExec = realExec
	} else {
		log.Info().Msg("模拟交易模式运行中")
		mainExec = execution.NewMockExecutor(*cfg, notifier, db, capitalMgr)
	}

	// 6. Token 注册表
	reg := registry.NewTokenRegistry()

	// 7. CEX 价格流
	cexStream := cexstream.NewStream()

	// 8. 事件驱动引擎
	taxDetector := engine.NewTaxDetector(ethClient, cfg.Chain.BSC)
	poolManager := engine.NewPoolManager(taxDetector)

	// 8.1 套利检测器
	detector := engine.NewArbitrageDetector(*cfg, poolManager, cexStream.FuturesCache, cexStream.SpotCache, reg, mainExec, notifier, binFuturesClient)

	// 8.2 CEX 价格变动 → 回调 detector
	cexStream.FuturesCache.OnUpdate = detector.OnCEXPriceUpdate

	// 8.3 LogWatcher (Sync 事件 → 回调 detector)
	wsURL := cfg.Chain.BSC.WsURL
	if wsURL == "" {
		wsURL = strings.Replace(cfg.Chain.BSC.RPC, "https://", "wss://", 1)
	}
	logWatcher := engine.NewLogWatcher(wsURL, poolManager, detector.OnPoolUpdate)

	// 9. 发现代币 & LP 池
	disc := discovery.NewBinanceDiscovery(cfg.Exchange.Binance, ethClient)
	tokens, err := disc.DiscoverBSCTokens(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("代币发现失败")
	}

	// 限制数量
	maxTokens := cfg.Scanner.MaxTokens
	if maxTokens > 0 && len(tokens) > maxTokens {
		tokens = tokens[:maxTokens]
	}

	// 注册代币到 Registry
	cexSymbols := make([]string, 0, len(tokens))
	for _, t := range tokens {
		reg.RegisterToken(t)
		cexSymbols = append(cexSymbols, t.CEXSymbol)
	}
	log.Info().Int("tokens", len(tokens)).Msg("代币注册完成")

	// 9.1 发现 LP 池地址
	pools := disc.DiscoverPools(ctx, tokens, cfg.Chain.BSC)

	// 9.2 注册池子到 PoolManager
	poolCount := 0
	for symbol, pool := range pools {
		token, ok := reg.GetToken(symbol)
		if !ok {
			continue
		}
		poolManager.AddPool(pool.PairAddress, pool.Token0, pool.Token1, symbol, token.ContractAddress, token.Decimals)
		poolCount++
	}
	log.Info().Int("v2_pools", poolCount).Msg("V2 LP 池注册完成")

	// 9.3 发现 V3 池子
	v3PoolCount := 0
	if cfg.Chain.BSC.FactoryV3 != "" {
		v3pools := disc.DiscoverPoolsV3(ctx, tokens, cfg.Chain.BSC)
		for symbol, pool := range v3pools {
			token, ok := reg.GetToken(symbol)
			if !ok {
				continue
			}
			poolManager.AddPoolV3(pool.PairAddress, pool.Token0, pool.Token1, symbol, token.ContractAddress, token.Decimals, pool.FeeTier)
			v3PoolCount++
		}
		log.Info().Int("v3_pools", v3PoolCount).Msg("V3 池注册完成")
	}
	poolCount += v3PoolCount

	// 9.4 保存到数据库
	if db != nil {
		for _, t := range tokens {
			dbToken := &storage.Token{
				Symbol:          t.Symbol,
				Name:            t.Name,
				Chain:           t.Chain,
				ContractAddress: t.ContractAddress,
				CEXSymbol:       t.CEXSymbol,
				Decimals:        t.Decimals,
				HasLiquidity:    t.HasLiquidity,
			}
			db.UpsertToken(dbToken)
		}
	}

	// 10. 启动 CEX WebSocket
	cexStream.SetSymbols(cexSymbols)
	go func() {
		if err := cexStream.Start(ctx); err != nil {
			log.Error().Err(err).Msg("CEX Stream 退出")
		}
	}()

	// 11. 启动 LogWatcher (事件驱动核心)
	go func() {
		if err := logWatcher.Start(ctx); err != nil {
			log.Error().Err(err).Msg("LogWatcher 退出")
		}
	}()

	log.Info().
		Int("tokens", len(tokens)).
		Int("pools", poolCount).
		Str("mode", cfg.Execution.Mode).
		Bool("live", *liveMode).
		Msg("CDEX 套利机器人已启动 (事件驱动)")

	notifier.Send("CDEX 机器人已启动\n" +
		"模式: " + cfg.Execution.Mode + "\n" +
		"代币: " + strconv.Itoa(len(tokens)) + "\n" +
		"LP池: " + strconv.Itoa(poolCount))

	// 12. 等待退出信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Info().Msg("正在停止机器人...")
	cexStream.Stop()
	cancel()
}

