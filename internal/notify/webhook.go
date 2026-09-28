package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Webhook flavors.
//
// A generic JSON POST silently fails on all three of the chat platforms a
// Chinese team is most likely to use — each has its own payload shape, and
// DingTalk additionally requires a signed query string. They return HTTP 200
// with an error code in the body when they reject a message, so the response
// has to be parsed rather than trusted.
const (
	FlavorGeneric  = "generic"
	FlavorDingTalk = "dingtalk"
	FlavorWeCom    = "wecom"
	FlavorFeishu   = "feishu"
)

// WebhookNotifier posts notifications to an HTTP endpoint.
type WebhookNotifier struct {
	url     string
	secret  string
	flavor  string
	client  *http.Client
	timeout time.Duration
}

// NewWebhookNotifier builds a webhook notifier. flavor must be one of the
// Flavor* constants; an unknown value falls back to generic.
func NewWebhookNotifier(rawURL, secret, flavor string, timeout time.Duration) *WebhookNotifier {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	switch flavor {
	case FlavorDingTalk, FlavorWeCom, FlavorFeishu:
	default:
		flavor = FlavorGeneric
	}
	return &WebhookNotifier{
		url: rawURL, secret: secret, flavor: flavor, timeout: timeout,
		client: &http.Client{Timeout: timeout},
	}
}

// Targets returns the single configured endpoint.
func (n *WebhookNotifier) Targets() []Target {
	return []Target{{Channel: "webhook", Address: n.url}}
}

// Send posts one batch.
func (n *WebhookNotifier) Send(ctx context.Context, t Target, events []Event) error {
	if len(events) == 0 {
		return nil
	}

	endpoint := t.Address
	// DingTalk requires the signature as query parameters on the URL itself.
	if n.flavor == FlavorDingTalk && n.secret != "" {
		signed, err := signDingTalk(endpoint, n.secret)
		if err != nil {
			return err
		}
		endpoint = signed
	}

	payload, err := n.buildPayload(events)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if n.flavor == FlavorGeneric && n.secret != "" {
		req.Header.Set("X-Exams-Signature", "sha256="+hmacHex(n.secret, payload))
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook post: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		_ = resp.Body.Close()
	}()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook http %s: %s", resp.Status, truncate(string(body), 200))
	}
	// These platforms answer 200 OK with an error code when the message is
	// rejected, so the status line alone is not evidence of delivery.
	return checkPlatformResponse(n.flavor, body)
}

func (n *WebhookNotifier) buildPayload(events []Event) ([]byte, error) {
	text := renderMarkdown(events)
	title := fmt.Sprintf("考试信息更新：%d 条", len(events))

	switch n.flavor {
	case FlavorDingTalk:
		return json.Marshal(map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]any{
				"title": title,
				"text":  text,
			},
		})
	case FlavorWeCom:
		return json.Marshal(map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"content": text},
		})
	case FlavorFeishu:
		return json.Marshal(map[string]any{
			"msg_type": "text",
			"content":  map[string]any{"text": truncate(text, 4000)},
		})
	default:
		return json.Marshal(map[string]any{
			"title":  title,
			"count":  len(events),
			"events": events,
		})
	}
}

// platformResponse covers the error envelope shared by DingTalk, WeCom and
// Feishu (they differ only in field names).
type platformResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	Code    int    `json:"code"`
	Msg     string `json:"msg"`
}

func checkPlatformResponse(flavor string, body []byte) error {
	if flavor == FlavorGeneric {
		return nil
	}
	var pr platformResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		// Not JSON: treat an unparsable 2xx as success rather than a failure.
		return nil
	}
	if pr.ErrCode != 0 {
		return fmt.Errorf("webhook rejected: errcode=%d errmsg=%s", pr.ErrCode, pr.ErrMsg)
	}
	if pr.Code != 0 {
		return fmt.Errorf("webhook rejected: code=%d msg=%s", pr.Code, pr.Msg)
	}
	return nil
}

// signDingTalk appends the timestamp and HMAC signature DingTalk requires.
func signDingTalk(endpoint, secret string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse webhook url: %w", err)
	}
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	q := u.Query()
	q.Set("timestamp", timestamp)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func hmacHex(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// ParseFlavor normalises a configured flavor name.
func ParseFlavor(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case FlavorDingTalk, "钉钉":
		return FlavorDingTalk
	case FlavorWeCom, "企业微信", "wechat":
		return FlavorWeCom
	case FlavorFeishu, "飞书", "lark":
		return FlavorFeishu
	default:
		return FlavorGeneric
	}
}
