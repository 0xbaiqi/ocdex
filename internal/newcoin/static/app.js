// NewCoin Arbitrage - Client-side JavaScript

var allCoins = [];
var costEstimateTimer = null;

document.addEventListener('DOMContentLoaded', function() {
    initSSE();
    initForms();
    initConfigForm();
    initTradingConfig();
    refreshPositions();
    fetchCostEstimate();
    initCoinSelector();
});

// ---- SSE price stream ----
function initSSE() {
    var evtSource = new EventSource('/sse');
    evtSource.onmessage = function(event) {
        var data = JSON.parse(event.data);
        updatePriceDisplay(data);
    };
    evtSource.onerror = function() {
        console.log('SSE connection lost, reconnecting...');
    };
}

function updatePriceDisplay(data) {
    var el = function(id) { return document.getElementById(id); };
    if (el('cex-price')) el('cex-price').textContent = '$' + data.cex_price;
    if (el('dex-price')) el('dex-price').textContent = '$' + data.dex_price;

    var spreadEl = el('spread-pct');
    if (spreadEl) {
        var spread = parseFloat(data.spread_pct);
        spreadEl.textContent = (spread >= 0 ? '+' : '') + data.spread_pct + '%';
        spreadEl.className = 'price-value ' + (spread >= 0 ? 'spread-positive' : 'spread-negative');
    }

    if (el('pool-liquidity')) el('pool-liquidity').textContent = '$' + numberWithCommas(data.liquidity);
    if (el('last-update')) el('last-update').textContent = data.timestamp;
}

// ---- Form submissions via fetch (for data-api forms) ----
function initForms() {
    document.querySelectorAll('form[data-api]').forEach(function(form) {
        form.addEventListener('submit', function(e) {
            e.preventDefault();
            var url = form.dataset.api;
            var btn = form.querySelector('button[type=submit]');
            var origText = btn.textContent;
            btn.disabled = true;
            btn.textContent = '处理中...';

            fetch(url, {
                method: 'POST',
                body: new FormData(form),
            })
            .then(function(r) { return r.json(); })
            .then(function(data) {
                showToast(data.message, data.status === 'ok' ? 'success' : 'error');
                if (data.status === 'ok') {
                    refreshPositions();
                    setTimeout(function() { location.reload(); }, 1500);
                }
            })
            .catch(function(err) {
                showToast('请求失败: ' + err.message, 'error');
            })
            .finally(function() {
                btn.disabled = false;
                btn.textContent = origText;
            });
        });
    });
}

// ---- Config form (auto-discover pools on save) ----
function initConfigForm() {
    var form = document.getElementById('config-form');
    if (!form) return;

    form.addEventListener('submit', function(e) {
        e.preventDefault();
        var btn = document.getElementById('save-config-btn');
        btn.disabled = true;
        btn.textContent = '正在发现池子...';

        var resultsEl = document.getElementById('pool-results');
        resultsEl.innerHTML = '<div class="coin-list-loading">正在保存配置并链上发现池子...</div>';

        fetch('/api/config', {
            method: 'POST',
            body: new FormData(form),
        })
        .then(function(r) { return r.json(); })
        .then(function(data) {
            if (data.status !== 'ok') {
                showToast(data.message, 'error');
                resultsEl.innerHTML = '<div class="coin-list-empty">' + escapeHtml(data.message) + '</div>';
                return;
            }

            showToast(data.message, 'success');

            // Show discovered pools
            var pools = (data.data && data.data.pools) || [];
            if (pools.length > 0) {
                renderPoolResults(resultsEl, pools);
            }

            // Reload after a short delay to reflect new state
            setTimeout(function() { location.reload(); }, 2000);
        })
        .catch(function(err) {
            showToast('请求失败: ' + err.message, 'error');
            resultsEl.innerHTML = '';
        })
        .finally(function() {
            btn.disabled = false;
            btn.textContent = '保存配置 (自动发现池子)';
        });
    });
}

// ---- Trading config (save settings, start/stop, cost estimate) ----
function initTradingConfig() {
    // Save settings button
    var saveBtn = document.getElementById('save-settings-btn');
    if (saveBtn) {
        saveBtn.addEventListener('click', function() {
            var form = document.getElementById('trading-config-form');
            saveBtn.disabled = true;
            saveBtn.textContent = '保存中...';

            fetch('/api/settings', {
                method: 'POST',
                body: new FormData(form),
            })
            .then(function(r) { return r.json(); })
            .then(function(data) {
                showToast(data.message, data.status === 'ok' ? 'success' : 'error');
            })
            .catch(function(err) {
                showToast('请求失败: ' + err.message, 'error');
            })
            .finally(function() {
                saveBtn.disabled = false;
                saveBtn.textContent = '保存设置';
            });
        });
    }

    // Start button (init positions + start monitor)
    var startBtn = document.getElementById('start-btn');
    if (startBtn) {
        startBtn.addEventListener('click', function() {
            if (!confirm('确认建仓并启动监控？将执行 CEX买入 + DEX买入 + 开空单')) return;
            var form = document.getElementById('trading-config-form');
            startBtn.disabled = true;
            startBtn.textContent = '建仓中...';

            fetch('/api/start', {
                method: 'POST',
                body: new FormData(form),
            })
            .then(function(r) { return r.json(); })
            .then(function(data) {
                showToast(data.message, data.status === 'ok' ? 'success' : 'error');
                if (data.status === 'ok') {
                    refreshPositions();
                    setTimeout(function() { location.reload(); }, 2000);
                }
            })
            .catch(function(err) {
                showToast('请求失败: ' + err.message, 'error');
            })
            .finally(function() {
                startBtn.disabled = false;
                startBtn.textContent = '一键建仓并启动';
            });
        });
    }

    // Stop button (close all positions + stop monitor)
    var stopBtn = document.getElementById('stop-btn');
    if (stopBtn) {
        stopBtn.addEventListener('click', function() {
            if (!confirm('确认平仓并停止？将卖出 CEX持仓 + DEX持仓 + 平空单')) return;
            stopBtn.disabled = true;
            stopBtn.textContent = '平仓中...';

            fetch('/api/stop', {
                method: 'POST',
            })
            .then(function(r) { return r.json(); })
            .then(function(data) {
                showToast(data.message, data.status === 'ok' ? 'success' : 'error');
                if (data.status === 'ok') {
                    refreshPositions();
                    setTimeout(function() { location.reload(); }, 2000);
                }
            })
            .catch(function(err) {
                showToast('请求失败: ' + err.message, 'error');
            })
            .finally(function() {
                stopBtn.disabled = false;
                stopBtn.textContent = '平仓停止';
            });
        });
    }

    // Close-all button (panic button - close all positions, keep monitor running)
    var closeAllBtn = document.getElementById('close-all-btn');
    if (closeAllBtn) {
        closeAllBtn.addEventListener('click', function() {
            if (!confirm('⚠ 确认一键清仓？\n\n将执行：\n1. 卖出 CEX 现货持仓\n2. 卖出 DEX 链上代币\n3. 平掉合约空单\n\n监控不会停止。')) return;
            closeAllBtn.disabled = true;
            closeAllBtn.textContent = '清仓中...';

            fetch('/api/close-all', {
                method: 'POST',
            })
            .then(function(r) { return r.json(); })
            .then(function(data) {
                showToast(data.message, data.status === 'ok' ? 'success' : 'error');
                if (data.status === 'ok') {
                    refreshPositions();
                    setTimeout(function() { location.reload(); }, 2000);
                }
            })
            .catch(function(err) {
                showToast('请求失败: ' + err.message, 'error');
            })
            .finally(function() {
                closeAllBtn.disabled = false;
                closeAllBtn.textContent = '⚠ 一键清仓';
            });
        });
    }

    // Cost estimate: debounce on trade amount change
    var tradeAmountInput = document.getElementById('trade-amount-input');
    if (tradeAmountInput) {
        tradeAmountInput.addEventListener('input', function() {
            if (costEstimateTimer) clearTimeout(costEstimateTimer);
            costEstimateTimer = setTimeout(fetchCostEstimate, 500);
        });
    }

    // Refresh positions button
    var refreshBtn = document.getElementById('refresh-pos-btn');
    if (refreshBtn) {
        refreshBtn.addEventListener('click', refreshPositions);
    }
}

// ---- Positions ----
function refreshPositions() {
    fetch('/api/positions')
        .then(function(r) { return r.json(); })
        .then(function(resp) {
            if (resp.status !== 'ok' || !resp.data) return;
            var d = resp.data;
            var el = function(id) { return document.getElementById(id); };

            if (el('pos-cex-usdt')) el('pos-cex-usdt').textContent = '$' + d.cex_usdt;
            if (el('pos-cex-token')) el('pos-cex-token').textContent = d.cex_token;
            if (el('pos-cex-token-usd')) el('pos-cex-token-usd').textContent = d.cex_token_usd ? '($' + d.cex_token_usd + ')' : '';
            if (el('pos-dex-usdt')) el('pos-dex-usdt').textContent = '$' + d.dex_usdt;
            if (el('pos-dex-token')) el('pos-dex-token').textContent = d.dex_token;
            if (el('pos-dex-token-usd')) el('pos-dex-token-usd').textContent = d.dex_token_usd ? '($' + d.dex_token_usd + ')' : '';

            var shortRow = el('pos-short-row');
            if (shortRow) {
                if (d.hedge_active) {
                    shortRow.style.display = 'flex';
                    if (el('pos-short-qty')) el('pos-short-qty').textContent = d.short_qty;
                    if (el('pos-short-entry')) el('pos-short-entry').textContent = '$' + d.short_entry;
                } else {
                    shortRow.style.display = 'none';
                }
            }

            var plEl = el('pos-realized-pl');
            if (plEl) {
                var pl = parseFloat(d.realized_pl) || 0;
                plEl.textContent = (pl >= 0 ? '+' : '') + '$' + pl.toFixed(2);
                plEl.className = 'pos-pl ' + (pl >= 0 ? 'pos-pl-positive' : 'pos-pl-negative');
            }
        })
        .catch(function() {});
}

// ---- Cost Estimate ----
function fetchCostEstimate() {
    var amountInput = document.getElementById('trade-amount-input');
    var amount = amountInput ? amountInput.value : '';

    fetch('/api/cost-estimate?amount=' + encodeURIComponent(amount))
        .then(function(r) { return r.json(); })
        .then(function(resp) {
            if (resp.status !== 'ok' || !resp.data) return;
            var d = resp.data;
            var el = function(id) { return document.getElementById(id); };

            if (el('cost-amount')) el('cost-amount').textContent = amount || '--';
            if (el('cost-dir-a')) {
                el('cost-dir-a').textContent = '$' + d.dir_a_total.toFixed(2) + ' (' + d.dir_a_min_spread.toFixed(2) + '%)';
            }
            if (el('cost-dir-b')) {
                el('cost-dir-b').textContent = '$' + d.dir_b_total.toFixed(2) + ' (' + d.dir_b_min_spread.toFixed(2) + '%)';
            }
        })
        .catch(function() {});
}

function renderPoolResults(container, pools) {
    var html = '<div class="pool-section">';
    html += '<div class="pool-section-title">已发现池子 (' + pools.length + ' 个)</div>';
    html += '<div class="pool-result-list">';
    pools.forEach(function(pool) {
        var tag = pool.version === 3 ? 'v3' : 'v2';
        var addr = pool.address.substring(0, 10) + '...' + pool.address.substring(pool.address.length - 8);
        var feeText = pool.version === 3 ? 'fee: ' + pool.fee_tier : '';
        html += '<div class="pool-result-item">'
            + '<span class="pool-tag ' + tag + '">V' + pool.version + '</span>'
            + '<span class="pool-quote">' + escapeHtml(pool.quote_token) + '</span>'
            + '<span class="pool-addr">' + addr + '</span>'
            + '<span class="pool-fee">' + feeText + '</span>'
            + '</div>';
    });
    html += '</div></div>';
    container.innerHTML = html;
}

function apiPost(url, body) {
    var formData = new FormData();
    if (body) {
        Object.keys(body).forEach(function(k) { formData.append(k, body[k]); });
    }
    return fetch(url, { method: 'POST', body: formData })
        .then(function(r) { return r.json(); })
        .then(function(data) {
            showToast(data.message, data.status === 'ok' ? 'success' : 'error');
            if (data.status === 'ok') {
                refreshPositions();
                setTimeout(function() { location.reload(); }, 1500);
            }
            return data;
        })
        .catch(function(err) {
            showToast('请求失败: ' + err.message, 'error');
        });
}

// ---- Coin Selector ----
function initCoinSelector() {
    var loadBtn = document.getElementById('load-coins-btn');
    var searchInput = document.getElementById('coin-search');
    var coinList = document.getElementById('coin-list');

    loadBtn.addEventListener('click', function() {
        loadBtn.disabled = true;
        loadBtn.textContent = '加载中...';
        coinList.innerHTML = '<div class="coin-list-loading">正在从币安加载币种列表...</div>';
        coinList.classList.add('show');

        fetch('/api/binance/coins')
            .then(function(r) { return r.json(); })
            .then(function(data) {
                if (data.status !== 'ok') {
                    coinList.innerHTML = '<div class="coin-list-empty">加载失败: ' + (data.message || 'unknown error') + '</div>';
                    return;
                }
                allCoins = data.coins || [];
                // Sort by 24h volume (descending)
                allCoins.sort(function(a, b) {
                    return parseFloat(b.volume_24h || 0) - parseFloat(a.volume_24h || 0);
                });
                searchInput.disabled = false;
                searchInput.focus();
                renderCoinList(allCoins);
                showToast('已加载 ' + allCoins.length + ' 个 BSC 币种', 'success');
            })
            .catch(function(err) {
                coinList.innerHTML = '<div class="coin-list-empty">加载失败: ' + err.message + '</div>';
            })
            .finally(function() {
                loadBtn.disabled = false;
                loadBtn.textContent = '刷新';
            });
    });

    searchInput.addEventListener('input', function() {
        var query = searchInput.value.trim().toUpperCase();
        if (!query) {
            renderCoinList(allCoins);
            return;
        }
        var filtered = allCoins.filter(function(c) {
            return c.symbol.toUpperCase().indexOf(query) !== -1 ||
                   c.name.toUpperCase().indexOf(query) !== -1;
        });
        renderCoinList(filtered);
    });
}

function renderCoinList(coins) {
    var coinList = document.getElementById('coin-list');
    if (coins.length === 0) {
        coinList.innerHTML = '<div class="coin-list-empty">没有找到匹配的币种</div>';
        coinList.classList.add('show');
        return;
    }

    // Show at most 50 coins to avoid DOM overload
    var displayed = coins.slice(0, 50);
    var html = '';
    displayed.forEach(function(coin) {
        var price = coin.price ? formatPrice(coin.price) : '--';
        var vol = coin.volume_24h ? formatVolume(coin.volume_24h) : '--';
        var changePct = parseFloat(coin.price_change_pct || 0);
        var changeClass = changePct >= 0 ? 'up' : 'down';
        var changeSign = changePct >= 0 ? '+' : '';

        html += '<div class="coin-item" data-coin=\'' + escapeHtml(JSON.stringify(coin)) + '\'>'
            + '<div class="coin-name">'
            + '<span class="coin-symbol">' + escapeHtml(coin.symbol) + '</span>'
            + '<span class="coin-fullname">' + escapeHtml(coin.name) + '</span>'
            + '</div>'
            + '<div class="coin-market">'
            + '<span class="coin-price">$' + price + '</span>'
            + '<span class="coin-change ' + changeClass + '">' + changeSign + changePct.toFixed(1) + '%</span>'
            + '<div class="coin-volume">Vol: $' + vol + '</div>'
            + '</div>'
            + '</div>';
    });

    if (coins.length > 50) {
        html += '<div class="coin-list-empty">显示前 50 个结果，请输入关键字搜索更多</div>';
    }

    coinList.innerHTML = html;
    coinList.classList.add('show');

    // Bind click events
    coinList.querySelectorAll('.coin-item').forEach(function(el) {
        el.addEventListener('click', function() {
            var coin = JSON.parse(el.dataset.coin);
            selectCoin(coin);
        });
    });
}

function selectCoin(coin) {
    // Show coin info
    var infoEl = document.getElementById('coin-info');
    infoEl.innerHTML = '<div class="coin-info-row"><span class="coin-info-label">币种</span><span class="coin-info-value">' + escapeHtml(coin.symbol) + ' (' + escapeHtml(coin.name) + ')</span></div>'
        + '<div class="coin-info-row"><span class="coin-info-label">交易对</span><span class="coin-info-value">' + escapeHtml(coin.cex_symbol) + '</span></div>'
        + '<div class="coin-info-row"><span class="coin-info-label">合约</span><span class="coin-info-value">' + escapeHtml(coin.contract_address) + '</span></div>'
        + '<div class="coin-info-row"><span class="coin-info-label">价格</span><span class="coin-info-value">$' + (coin.price || '--') + '</span></div>'
        + '<div class="coin-info-row"><span class="coin-info-label">24h 成交额</span><span class="coin-info-value">$' + formatVolume(coin.volume_24h || '0') + '</span></div>'
        + '<div class="coin-info-row"><span class="coin-info-label">充值/提现</span><span class="coin-info-value">'
        + (coin.deposit_enabled ? '充值可用' : '充值关闭') + ' / '
        + (coin.withdraw_enabled ? '提现可用' : '提现关闭') + '</span></div>';
    infoEl.classList.add('show');

    // Hide coin list
    document.getElementById('coin-list').classList.remove('show');

    // Auto-fill config form (only basic fields)
    var form = document.getElementById('config-form');
    if (form) {
        setFormValue(form, 'symbol', coin.symbol);
        setFormValue(form, 'cex_symbol', coin.cex_symbol);
        setFormValue(form, 'contract_address', coin.contract_address);
        setFormValue(form, 'cex_multiplier', coin.cex_multiplier || 1);
    }

    // Clear previous pool results
    document.getElementById('pool-results').innerHTML = '';

    showToast('已选择 ' + coin.symbol + '，请点击"保存配置"自动发现池子', 'success');
}

// ---- Helpers ----
function showToast(msg, type) {
    var toast = document.createElement('div');
    toast.className = 'toast ' + type;
    toast.textContent = msg;
    document.body.appendChild(toast);
    setTimeout(function() { toast.remove(); }, 3000);
}

function numberWithCommas(x) {
    return x.toString().replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

function formatPrice(priceStr) {
    var p = parseFloat(priceStr);
    if (isNaN(p)) return priceStr;
    if (p >= 1) return p.toFixed(2);
    if (p >= 0.01) return p.toFixed(4);
    return p.toFixed(6);
}

function formatVolume(volStr) {
    var v = parseFloat(volStr);
    if (isNaN(v)) return volStr;
    if (v >= 1e9) return (v / 1e9).toFixed(1) + 'B';
    if (v >= 1e6) return (v / 1e6).toFixed(1) + 'M';
    if (v >= 1e3) return (v / 1e3).toFixed(1) + 'K';
    return v.toFixed(0);
}

function setFormValue(form, name, value) {
    var el = form.querySelector('[name=' + name + ']');
    if (el) el.value = value;
}

function escapeHtml(str) {
    var div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
}
