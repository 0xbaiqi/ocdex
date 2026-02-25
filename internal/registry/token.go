package registry

import (
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// TokenInfo 币种信息
type TokenInfo struct {
	Symbol          string
	Name            string
	Chain           string
	ContractAddress string
	CEXSymbol       string // e.g., BNBUSDT
	Decimals        int
	CEXMultiplier   int64  // 币安缩写倍率: 1MBABYDOGE→1000000, 1000SATS→1000, 普通币→1
	HasLiquidity    bool
	UpdatedAt       time.Time
}

// ParseCEXMultiplier 从币安 Symbol 解析缩写倍率
// 例: "1MBABYDOGE" → 1000000, "1000SATS" → 1000, "BNB" → 1
func ParseCEXMultiplier(symbol string) int64 {
	if strings.HasPrefix(symbol, "1M") {
		return 1_000_000
	}
	if strings.HasPrefix(symbol, "1000") {
		return 1000
	}
	if strings.HasPrefix(symbol, "100") {
		return 100
	}
	if strings.HasPrefix(symbol, "10") {
		// 避免误匹配正常币名，检查下一个字符是否是大写字母
		rest := symbol[2:]
		if len(rest) > 0 && rest[0] >= 'A' && rest[0] <= 'Z' {
			return 10
		}
	}
	return 1
}

// TokenPrice 币种价格
type TokenPrice struct {
	Symbol       string
	CEXPrice     decimal.Decimal
	DEXPrice     decimal.Decimal
	CEXUpdatedAt time.Time
	DEXUpdatedAt time.Time
	Spread       decimal.Decimal // (CEX - DEX) / DEX * 100%
	HasLiquidity bool
}

// TokenRegistry 币种注册表 (内存缓存)
type TokenRegistry struct {
	mu     sync.RWMutex
	tokens map[string]*TokenInfo  // key: symbol
	prices map[string]*TokenPrice // key: symbol
}

// NewTokenRegistry 创建币种注册表
func NewTokenRegistry() *TokenRegistry {
	return &TokenRegistry{
		tokens: make(map[string]*TokenInfo),
		prices: make(map[string]*TokenPrice),
	}
}

// RegisterToken 注册币种
func (r *TokenRegistry) RegisterToken(info *TokenInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[info.Symbol] = info
	// 初始化价格
	if _, exists := r.prices[info.Symbol]; !exists {
		r.prices[info.Symbol] = &TokenPrice{
			Symbol:       info.Symbol,
			HasLiquidity: info.HasLiquidity,
		}
	}
}

// GetToken 获取币种信息
func (r *TokenRegistry) GetToken(symbol string) (*TokenInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.tokens[symbol]
	return info, ok
}

// GetAllTokens 获取所有币种
func (r *TokenRegistry) GetAllTokens() []*TokenInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*TokenInfo, 0, len(r.tokens))
	for _, info := range r.tokens {
		result = append(result, info)
	}
	return result
}

// GetTokensWithLiquidity 获取有流动性的币种
func (r *TokenRegistry) GetTokensWithLiquidity() []*TokenInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*TokenInfo, 0)
	for _, info := range r.tokens {
		if info.HasLiquidity {
			result = append(result, info)
		}
	}
	return result
}

// UpdateCEXPrice 更新 CEX 价格
func (r *TokenRegistry) UpdateCEXPrice(symbol string, price decimal.Decimal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.prices[symbol]; ok {
		p.CEXPrice = price
		p.CEXUpdatedAt = time.Now()
		r.updateSpread(p)
	}
}

// UpdateDEXPrice 更新 DEX 价格
func (r *TokenRegistry) UpdateDEXPrice(symbol string, price decimal.Decimal, hasLiquidity bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.prices[symbol]; ok {
		p.DEXPrice = price
		p.DEXUpdatedAt = time.Now()
		p.HasLiquidity = hasLiquidity
		r.updateSpread(p)
	}
}

// updateSpread 计算差价
func (r *TokenRegistry) updateSpread(p *TokenPrice) {
	if p.DEXPrice.IsZero() {
		p.Spread = decimal.Zero
		return
	}
	// Spread = (CEX - DEX) / DEX * 100
	p.Spread = p.CEXPrice.Sub(p.DEXPrice).Div(p.DEXPrice).Mul(decimal.NewFromInt(100))
}

// GetPrice 获取价格
func (r *TokenRegistry) GetPrice(symbol string) (*TokenPrice, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.prices[symbol]
	if !ok {
		return nil, false
	}
	// 返回副本
	copy := *p
	return &copy, true
}

// GetAllPrices 获取所有价格
func (r *TokenRegistry) GetAllPrices() []*TokenPrice {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*TokenPrice, 0, len(r.prices))
	for _, p := range r.prices {
		copy := *p
		result = append(result, &copy)
	}
	return result
}

// GetPricesWithSpread 获取有差价的价格 (按差价排序)
func (r *TokenRegistry) GetPricesWithSpread(minSpread decimal.Decimal) []*TokenPrice {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*TokenPrice, 0)
	for _, p := range r.prices {
		if !p.HasLiquidity {
			continue
		}
		if p.CEXPrice.IsZero() || p.DEXPrice.IsZero() {
			continue
		}
		absSpread := p.Spread.Abs()
		if absSpread.GreaterThanOrEqual(minSpread) {
			copy := *p
			result = append(result, &copy)
		}
	}
	return result
}

// TokenCount 获取注册的币种数量
func (r *TokenRegistry) TokenCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tokens)
}

// MarkNoLiquidity 标记无流动性
func (r *TokenRegistry) MarkNoLiquidity(symbol string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if info, ok := r.tokens[symbol]; ok {
		info.HasLiquidity = false
	}
	if p, ok := r.prices[symbol]; ok {
		p.HasLiquidity = false
	}
}
