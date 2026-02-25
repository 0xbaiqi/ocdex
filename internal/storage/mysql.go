package storage

import (
	"database/sql"
	"fmt"
	"time"

	"ocdex/config"

	"github.com/shopspring/decimal"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Token 币种注册表
type Token struct {
	ID              uint      `gorm:"primaryKey"`
	Symbol          string    `gorm:"size:20;not null;index"`
	Name            string    `gorm:"size:100"`
	Chain           string    `gorm:"size:20;not null;default:BSC"`
	ContractAddress string    `gorm:"size:66;not null"`
	CEXSymbol       string    `gorm:"size:20"`
	Decimals        int       `gorm:"default:18"`
	HasLiquidity    bool      `gorm:"default:true"`
	CreatedAt       time.Time `gorm:"autoCreateTime"`
	UpdatedAt       time.Time `gorm:"autoUpdateTime"`
}

func (Token) TableName() string {
	return "tokens"
}

// Opportunity 套利机会记录
type Opportunity struct {
	ID             uint            `gorm:"primaryKey"`
	TokenID        uint            `gorm:"index"`
	Symbol         string          `gorm:"size:20;not null;index"`
	Chain          string          `gorm:"size:20;not null;default:BSC"`
	CEXPrice       decimal.Decimal `gorm:"type:decimal(24,8);not null"`
	DEXPrice       decimal.Decimal `gorm:"type:decimal(24,8);not null"`
	SpreadPercent  decimal.Decimal `gorm:"type:decimal(24,4);not null"`
	GrossProfitUSD decimal.Decimal `gorm:"type:decimal(16,4)"`
	GasCostUSD     decimal.Decimal `gorm:"type:decimal(16,4)"`
	FeeCostUSD     decimal.Decimal `gorm:"type:decimal(16,4)"`
	NetProfitUSD   decimal.Decimal `gorm:"type:decimal(16,4)"`
	Direction      string          `gorm:"size:30;not null"` // BUY_DEX_SELL_CEX, BUY_CEX_SELL_DEX
	Status         string          `gorm:"size:20;default:DETECTED;index"`
	DEXTxHash      string          `gorm:"size:66"`
	CEXOrderID     string          `gorm:"size:50"`
	ActualProfit   decimal.Decimal `gorm:"type:decimal(16,4)"`
	DetectedAt     time.Time       `gorm:"autoCreateTime;index"`
	ExecutedAt     *time.Time
}

func (Opportunity) TableName() string {
	return "opportunities"
}

// PriceHistory 价格历史记录 (用于回测分析)
type PriceHistory struct {
	ID           uint            `gorm:"primaryKey"`
	TokenID      uint            `gorm:"index:idx_token_time"`
	Symbol       string          `gorm:"size:20;not null"`
	Chain        string          `gorm:"size:20;not null;default:BSC"`
	CEXPrice     decimal.Decimal `gorm:"type:decimal(24,8)"`
	DEXPrice     decimal.Decimal `gorm:"type:decimal(24,8)"`
	GasPriceGwei decimal.Decimal `gorm:"type:decimal(10,2)"`
	RecordedAt   time.Time       `gorm:"autoCreateTime;index:idx_token_time"`
}

func (PriceHistory) TableName() string {
	return "price_history"
}

// TradeHistory 详细交易日志 (Phase 4)
type TradeHistory struct {
	ID        uint   `gorm:"primaryKey"`
	TradeID   string `gorm:"size:36;not null;uniqueIndex"`
	Symbol    string `gorm:"size:20;not null;index"`
	Direction string `gorm:"size:30;not null"`
	Mode      string `gorm:"size:30;not null"`

	// 价格快照
	DEXPrice      decimal.Decimal `gorm:"type:decimal(24,8)"`
	CEXPrice      decimal.Decimal `gorm:"type:decimal(24,8)"`
	SpreadPercent decimal.Decimal `gorm:"type:decimal(10,4)"`

	// 金额
	AmountUSD decimal.Decimal `gorm:"type:decimal(20,8)"`
	Quantity  decimal.Decimal `gorm:"type:decimal(20,8)"`

	// 费用明细
	GasCostUSD    decimal.Decimal `gorm:"type:decimal(20,8)"`
	CEXFeeUSD     decimal.Decimal `gorm:"type:decimal(20,8)"`
	FuturesFeeUSD decimal.Decimal `gorm:"type:decimal(20,8)"`
	TotalFeeUSD   decimal.Decimal `gorm:"type:decimal(20,8)"`

	// 利润
	GrossProfit decimal.Decimal `gorm:"type:decimal(20,8)"`
	NetProfit   decimal.Decimal `gorm:"type:decimal(20,8)"`

	// 时间节点
	DetectedAt   time.Time    `gorm:"precision:3"`
	DEXBuyAt     sql.NullTime `gorm:"precision:3"`
	ShortOpenAt  sql.NullTime `gorm:"precision:3"`
	TransferAt   sql.NullTime `gorm:"precision:3"`
	DepositAt    sql.NullTime `gorm:"precision:3"`
	SpotSellAt   sql.NullTime `gorm:"precision:3"`
	ShortCloseAt sql.NullTime `gorm:"precision:3"`
	CompletedAt  sql.NullTime `gorm:"precision:3"`

	// 交易哈希/订单号
	DEXTxHash     string `gorm:"size:66"`
	TransferTx    string `gorm:"size:66"`
	CEXSpotOrder  string `gorm:"size:50"`
	CEXShortOrder string `gorm:"size:50"`
	CEXCloseOrder string `gorm:"size:50"`

	// 状态
	Status   string `gorm:"size:20;default:PENDING;index"`
	ErrorMsg string `gorm:"type:text"`

	CreatedAt time.Time `gorm:"autoCreateTime"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`
}

func (TradeHistory) TableName() string {
	return "trade_history"
}

// MySQL 数据库存储
type MySQL struct {
	db *gorm.DB
}

// NewMySQL 创建 MySQL 连接
func NewMySQL(cfg config.DatabaseConfig) (*MySQL, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		cfg.Username, cfg.Password, cfg.Host, cfg.Port, cfg.Database)

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(time.Hour)

	// 自动迁移表结构 (新增 TradeHistory)
	if err := db.AutoMigrate(&Token{}, &Opportunity{}, &PriceHistory{}, &TradeHistory{}); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	return &MySQL{db: db}, nil
}

// SaveToken 保存或更新币种
func (m *MySQL) SaveToken(token *Token) error {
	return m.db.Save(token).Error
}

// GetTokenBySymbol 按符号查找币种
func (m *MySQL) GetTokenBySymbol(chain, symbol string) (*Token, error) {
	var token Token
	err := m.db.Where("chain = ? AND symbol = ?", chain, symbol).First(&token).Error
	if err != nil {
		return nil, err
	}
	return &token, nil
}

// GetAllTokens 获取所有币种
func (m *MySQL) GetAllTokens(chain string) ([]Token, error) {
	var tokens []Token
	err := m.db.Where("chain = ? AND has_liquidity = ?", chain, true).Find(&tokens).Error
	return tokens, err
}

// UpsertToken 插入或更新币种
func (m *MySQL) UpsertToken(token *Token) error {
	return m.db.Where("chain = ? AND contract_address = ?", token.Chain, token.ContractAddress).
		Assign(token).FirstOrCreate(token).Error
}

// SaveOpportunity 保存套利机会
func (m *MySQL) SaveOpportunity(opp *Opportunity) error {
	return m.db.Create(opp).Error
}

// UpdateOpportunityStatus 更新机会状态
func (m *MySQL) UpdateOpportunityStatus(id uint, status string, txHash, orderID string, actualProfit decimal.Decimal) error {
	now := time.Now()
	return m.db.Model(&Opportunity{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":        status,
		"dex_tx_hash":   txHash,
		"cex_order_id":  orderID,
		"actual_profit": actualProfit,
		"executed_at":   &now,
	}).Error
}

// SaveTradeHistory 保存详细交易日志
func (m *MySQL) SaveTradeHistory(trade *TradeHistory) error {
	return m.db.Create(trade).Error
}

// UpdateTradeHistory 更新详细交易日志
func (m *MySQL) UpdateTradeHistory(trade *TradeHistory) error {
	return m.db.Save(trade).Error
}

// GetRecentOpportunities 获取最近的机会
func (m *MySQL) GetRecentOpportunities(limit int) ([]Opportunity, error) {
	var opps []Opportunity
	err := m.db.Order("detected_at DESC").Limit(limit).Find(&opps).Error
	return opps, err
}

// SavePriceHistory 保存价格历史
func (m *MySQL) SavePriceHistory(history *PriceHistory) error {
	return m.db.Create(history).Error
}

// Close 关闭数据库连接
func (m *MySQL) Close() error {
	sqlDB, err := m.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
