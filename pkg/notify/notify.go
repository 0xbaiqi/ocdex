package notify

import (
	"bytes"
	"ocdex/config"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

type Notifier interface {
	Name() string
	Send(msg string) error
}

type MultiNotifier struct {
	notifiers []Notifier
}

func NewMultiNotifier(cfg config.NotifyConfig) *MultiNotifier {
	mn := &MultiNotifier{
		notifiers: make([]Notifier, 0),
	}

	// 飞书
	if cfg.Feishu.Enabled {
		mn.notifiers = append(mn.notifiers, &FeishuNotifier{
			WebhookURL: "https://open.larksuite.com/open-apis/bot/v2/hook/" + cfg.Feishu.Token,
		})
	}

	// Telegram (新配置格式)
	if cfg.Telegram.Enabled && cfg.Telegram.BotToken != "" {
		for _, chatID := range cfg.Telegram.ChatIDs {
			mn.notifiers = append(mn.notifiers, &TelegramNotifier{
				Token:  cfg.Telegram.BotToken,
				ChatID: chatID,
			})
		}
	}

	// Webhooks
	for _, wh := range cfg.Webhooks {
		if wh.Enabled {
			mn.notifiers = append(mn.notifiers, &WebhookNotifier{
				name:    wh.Name,
				URL:     wh.URL,
				Method:  wh.Method,
				Headers: wh.Headers,
			})
		}
	}

	return mn
}

func (mn *MultiNotifier) Send(msg string) {
	for _, n := range mn.notifiers {
		go func(notifier Notifier) {
			if err := notifier.Send(msg); err != nil {
				log.Error().Err(err).Str("notifier", notifier.Name()).Msg("通知发送失败")
			}
		}(n)
	}
}

// Register 注册新的通知器
func (mn *MultiNotifier) Register(n Notifier) {
	mn.notifiers = append(mn.notifiers, n)
}

// Feishu 飞书通知
type FeishuNotifier struct {
	WebhookURL string
}

func (f *FeishuNotifier) Name() string { return "feishu" }

func (f *FeishuNotifier) Send(msg string) error {
	payload := map[string]interface{}{
		"msg_type": "text",
		"content": map[string]string{
			"text": msg,
		},
	}
	return postJSON(f.WebhookURL, payload, nil)
}

// Telegram 通知
type TelegramNotifier struct {
	Token  string
	ChatID string
}

func (t *TelegramNotifier) Name() string { return "telegram" }

func (t *TelegramNotifier) Send(msg string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.Token)
	// Escape HTML special chars to prevent parse failures from error messages
	escapedMsg := htmlEscape(msg)
	payload := map[string]string{
		"chat_id":    t.ChatID,
		"text":       escapedMsg,
		"parse_mode": "HTML",
	}
	return postJSON(url, payload, nil)
}

// Webhook 自定义 HTTP 回调
type WebhookNotifier struct {
	name    string
	URL     string
	Method  string
	Headers map[string]string
}

func (w *WebhookNotifier) Name() string { return w.name }

func (w *WebhookNotifier) Send(msg string) error {
	payload := map[string]interface{}{
		"message":   msg,
		"timestamp": time.Now().Unix(),
	}

	method := w.Method
	if method == "" {
		method = "POST"
	}

	return postJSONWithMethod(w.URL, method, payload, w.Headers)
}

// htmlEscape escapes HTML special characters to prevent Telegram parse errors.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func postJSON(url string, data interface{}, headers map[string]string) error {
	return postJSONWithMethod(url, "POST", data, headers)
}

func postJSONWithMethod(url, method string, data interface{}, headers map[string]string) error {
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("notification failed with status: %d, body: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
