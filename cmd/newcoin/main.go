package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"ocdex/config"
	"ocdex/external/cex"
	"ocdex/internal/newcoin"
	"ocdex/pkg/logger"
	"ocdex/pkg/notify"
	"ocdex/pkg/wallet"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
)

var (
	addr      = flag.String("addr", ":8080", "HTTP listen address")
	stateFile = flag.String("state", "data/newcoin.json", "State file path")
)

func main() {
	flag.Parse()

	// 1. Load config
	cfg, err := config.LoadConfig("config")
	if err != nil {
		panic("config load failed: " + err.Error())
	}

	// 2. Logger
	logger.InitLogger(cfg.Log.Level)
	log.Info().Msg("NewCoin Arbitrage Tool starting...")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 3. ETH client
	ethClient, err := ethclient.Dial(cfg.Chain.BSC.RPC)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to connect BSC node")
	}

	// 4. Wallet
	wm, err := wallet.NewWalletManager(cfg.Wallet.Mnemonic)
	if err != nil {
		log.Fatal().Err(err).Msg("Wallet init failed")
	}
	log.Info().Str("wallet", wm.GetAddress()).Msg("Wallet ready")

	// 5. CEX clients
	binClient := cex.NewBinanceClient(cfg.Exchange.Binance)
	binFutures := cex.NewBinanceFuturesClient(cfg.Exchange.Binance)

	// 6. Notifier
	notifier := notify.NewMultiNotifier(cfg.Notify)

	// 7. NewCoin components
	state := newcoin.NewStateManager(*stateFile)
	binAPI := newcoin.NewBinanceAPI(cfg.Exchange.Binance, ethClient, cfg.Chain.BSC)
	aggregator := newcoin.NewDEXAggregator(ethClient, cfg.Chain.BSC.USDT)
	monitor := newcoin.NewMonitor(cfg, binAPI, aggregator)

	strategy, err := newcoin.NewStrategy(
		cfg, state, monitor,
		binClient, binFutures, ethClient,
		wm.PrivateKey, wm.Address,
		notifier,
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Strategy init failed")
	}

	autoTrader := newcoin.NewAutoTrader(strategy, monitor, state)

	server := newcoin.NewServer(cfg, state, monitor, strategy, binAPI, autoTrader)

	// 8. Restore previous monitoring state
	if tc := state.GetState().Token; tc != nil {
		log.Info().Str("symbol", tc.Symbol).Msg("Restoring previous token config")
		monitor.Configure(tc)
		go func() {
			if err := monitor.Start(ctx); err != nil {
				log.Error().Err(err).Msg("Monitor exited")
			}
		}()

		// Auto-start AutoTrader if AutoTrade was enabled
		if state.GetState().Settings.AutoTrade {
			log.Info().Msg("Restoring AutoTrader")
			go autoTrader.Start(ctx)
		}
	}

	// 9. Start web server
	go func() {
		if err := server.Start(*addr); err != nil {
			log.Fatal().Err(err).Msg("Web server failed")
		}
	}()

	log.Info().Str("addr", *addr).Msg("NewCoin Arbitrage Tool ready")
	notifier.Send("NewCoin Arbitrage Tool started\nWeb UI: http://localhost" + *addr)

	// 10. Wait for exit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Info().Msg("Shutting down...")
	autoTrader.Stop()
	monitor.Stop()
	cancel()
}
