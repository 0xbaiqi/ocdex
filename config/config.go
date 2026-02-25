package config

import (
	"strings"

	"github.com/shopspring/decimal"
	"github.com/spf13/viper"
)

type Config struct {
	App       AppConfig       `mapstructure:"app"`
	Log       LogConfig       `mapstructure:"log"`
	Notify    NotifyConfig    `mapstructure:"notify"`
	Exchange  ExchangeConfig  `mapstructure:"exchange"`
	Strategy  StrategyConfig  `mapstructure:"strategy"`
	Execution ExecutionConfig `mapstructure:"execution"`
	Wallet    WalletConfig    `mapstructure:"wallet"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Scanner   ScannerConfig   `mapstructure:"scanner"`
	Chain     ChainConfig     `mapstructure:"chain"`
	Redis     RedisConfig     `mapstructure:"redis"`
}

type WalletConfig struct {
	Mnemonic string `mapstructure:"mnemonic"`
}

type StrategyConfig struct {
	MinProfitUSD    float64 `mapstructure:"min_profit_usd"`
	MinProfitRate   float64 `mapstructure:"min_profit_rate"`
	MaxGasPriceGwei float64 `mapstructure:"max_gas_price_gwei"`
	Slippage        float64 `mapstructure:"slippage"` // in percentage e.g. 0.5
	TradeAmountUSD  float64 `mapstructure:"trade_amount_usd"`
	PollInterval    int     `mapstructure:"poll_interval_ms"`
	CexFeeRate      float64 `mapstructure:"cex_fee_rate"` // e.g., 0.001 (0.1%)
	DexFeeRate      float64 `mapstructure:"dex_fee_rate"` // e.g., 0.0025 (0.25%)
	// Capital Management
	TotalCapitalUSD     float64 `mapstructure:"total_capital_usd"`
	PerTradeUSD         float64 `mapstructure:"per_trade_usd"`
	MaxConcurrentTrades int     `mapstructure:"max_concurrent_trades"`
	// Futures Specific
	Futures FuturesConfig `mapstructure:"futures"`
}

type AppConfig struct {
	Env string `mapstructure:"env"` // dev, prod
}

type LogConfig struct {
	Level string `mapstructure:"level"` // debug, info, warn, error
}

type NotifyConfig struct {
	Telegram TelegramConfig  `mapstructure:"telegram"`
	Feishu   TokenConfig     `mapstructure:"feishu"`
	Webhooks []WebhookConfig `mapstructure:"webhooks"`
}

type TelegramConfig struct {
	Enabled  bool     `mapstructure:"enabled"`
	BotToken string   `mapstructure:"bot_token"`
	ChatIDs  []string `mapstructure:"chat_ids"`
}

type TokenConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Token   string `mapstructure:"token"`
	ChatID  string `mapstructure:"chat_id"` // For Telegram (legacy)
}

type WebhookConfig struct {
	Name    string            `mapstructure:"name"`
	URL     string            `mapstructure:"url"`
	Method  string            `mapstructure:"method"`
	Headers map[string]string `mapstructure:"headers"`
	Enabled bool              `mapstructure:"enabled"`
}

type ExchangeConfig struct {
	Binance APIConfig `mapstructure:"binance"`
	OKX     APIConfig `mapstructure:"okx"`
	// Fees per exchange
	Fees map[string]FeeConfig `mapstructure:"fees"`
}

type FeeConfig struct {
	SpotRate          float64 `mapstructure:"spot_rate"`
	FuturesRate       float64 `mapstructure:"futures_rate"`        // 向后兼容
	FuturesMakerRate  float64 `mapstructure:"futures_maker_rate"`  // 合约挂单费率
	FuturesTakerRate  float64 `mapstructure:"futures_taker_rate"`  // 合约吃单费率
	WithdrawalCost    float64 `mapstructure:"withdrawal_cost"`
}

type APIConfig struct {
	ApiKey    string `mapstructure:"api_key"`
	SecretKey string `mapstructure:"secret_key"`
}

type DatabaseConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	Host         string `mapstructure:"host"`
	Port         int    `mapstructure:"port"`
	Username     string `mapstructure:"username"`
	Password     string `mapstructure:"password"`
	Database     string `mapstructure:"db"`
	MaxIdleConns int    `mapstructure:"max_idle_conns"`
	MaxOpenConns int    `mapstructure:"max_open_conns"`
}

type ScannerConfig struct {
	Enabled          bool    `mapstructure:"enabled"`
	PollIntervalMs   int     `mapstructure:"poll_interval_ms"`
	MinSpreadPercent float64 `mapstructure:"min_spread_percent"`
	MinProfitUSD     float64 `mapstructure:"min_profit_usd"`
	BatchSize        int     `mapstructure:"batch_size"`
	MaxTokens        int     `mapstructure:"max_tokens"`        // 最大监控币种数
	MinLiquidityUSD  float64 `mapstructure:"min_liquidity_usd"` // 最小流动性要求
}

type ChainConfig struct {
	BSC BSCConfig `mapstructure:"bsc"`
}

type RedisConfig struct {
	Host     string `mapstructure:"host"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type FuturesConfig struct {
	MarginType string `mapstructure:"margin_type"` // ISOLATED, CROSSED
	Leverage   int    `mapstructure:"leverage"`
}

type BSCConfig struct {
	Enabled   bool   `mapstructure:"enabled"`
	RPC       string `mapstructure:"rpc"`
	WsURL     string `mapstructure:"ws_url"`
	Multicall string `mapstructure:"multicall"`
	RouterV2  string `mapstructure:"router_v2"`
	FactoryV2 string `mapstructure:"factory_v2"`
	FactoryV3 string `mapstructure:"factory_v3"`
	WBNB      string `mapstructure:"wbnb"`
	USDT      string `mapstructure:"usdt"`
}

type ExecutionConfig struct {
	Mode string        `mapstructure:"mode"`
	Web  WebConfig     `mapstructure:"web"`
	Dep  DepositConfig `mapstructure:"deposit"`
}

type WebConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Port    int    `mapstructure:"port"`
	Host    string `mapstructure:"host"`
}

type DepositConfig struct {
	Address         string `mapstructure:"address"`
	Memo            string `mapstructure:"memo"`
	PollIntervalSec int    `mapstructure:"poll_interval_sec"`
	TimeoutMinutes  int    `mapstructure:"timeout_minutes"`
}

// Strategy Mode Constants
const (
	ModeInventorySpot = "INVENTORY_SPOT"
	ModeDepositSell   = "DEPOSIT_AND_SELL"
	ModeFuturesHedge  = "FUTURES_HEDGE"
)

func LoadConfig(path string) (*Config, error) {
	v := viper.New()

	// 允许通过环境变量指定配置文件名，例如 CDEX_CONFIG_NAME=config-pro
	configName := "config"
	v.SetEnvPrefix("CDEX")
	v.AutomaticEnv()
	if envConfigName := v.GetString("CONFIG_NAME"); envConfigName != "" {
		configName = envConfigName
	}

	v.SetConfigName(configName)
	v.SetConfigType("yaml")
	v.AddConfigPath(path)
	v.AddConfigPath(".")

	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func NewDecimal(f float64) decimal.Decimal {
	return decimal.NewFromFloat(f)
}
