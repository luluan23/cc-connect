package xiezuo

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"

	openevent "github.com/GongchuangSu/open-event-sdk-go"
)

const (
	apiBaseURL  = "https://openapi.wps.cn"
	kso1Type    = "KSO-1"
	contentType = "application/json"
)

func init() {
	core.RegisterPlatform("xiezuo", New)
}

type replyContext struct {
	chatID    string
	messageID string
}

// Platform implements core.Platform for WPS Xiezuo.
type Platform struct {
	appID    string
	appKey   string
	handler  core.MessageHandler
	wsClient *openevent.Client

	// OAuth2 access token cache
	tokenMu     sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

func New(opts map[string]any) (core.Platform, error) {
	appID := os.Getenv("XIEZUO_APP_ID")
	appKey := os.Getenv("XIEZUO_APP_KEY")
	if appID == "" || appKey == "" {
		return nil, fmt.Errorf("xiezuo: XIEZUO_APP_ID and XIEZUO_APP_KEY environment variables are required")
	}
	return &Platform{
		appID:  appID,
		appKey: appKey,
	}, nil
}

func (p *Platform) Name() string { return "xiezuo" }

func (p *Platform) Start(handler core.MessageHandler) error {
	p.handler = handler

	dispatcher := openevent.NewDispatcher()

	dispatcher.OnV7AppChatMessageCreate(func(ctx context.Context, e *openevent.V7AppChatMessageCreateEvent) error {
		chatID := e.Data.Chat.Id
		messageID := e.Data.Message.Id
		senderID := e.Data.Sender.Id
		senderName := e.Data.Sender.Name

		if e.Data.SendTime > 0 {
			msgTime := time.Unix(e.Data.SendTime, 0)
			if core.IsOldMessage(msgTime) {
				slog.Debug("xiezuo: ignoring old message", "chat_id", chatID, "message_id", messageID)
				return nil
			}
		}

		// Only handle text messages for now
		if e.Data.Message.Type != "text" {
			slog.Debug("xiezuo: ignoring non-text message", "type", e.Data.Message.Type)
			return nil
		}

		text := extractTextContent(e.Data.Message.Content)
		if text == "" {
			return nil
		}

		slog.Debug("xiezuo: message received", "chat_id", chatID, "sender", senderID, "text_len", len(text))

		sessionKey := fmt.Sprintf("xiezuo:%s", chatID)
		p.handler(p, &core.Message{
			SessionKey: sessionKey,
			Platform:   "xiezuo",
			MessageID:  messageID,
			UserID:     senderID,
			UserName:   senderName,
			Content:    text,
			ReplyCtx:   replyContext{chatID: chatID, messageID: messageID},
		})
		return nil
	})

	dispatcher.RegisterFallbackFunc(func(ctx context.Context, e *openevent.Event) error {
		slog.Debug("xiezuo: unhandled event", "event_code", e.EventCode())
		return nil
	})

	p.wsClient = openevent.NewClient(p.appID, p.appKey,
		openevent.WithDispatcher(dispatcher),
	)

	go func() {
		slog.Info("xiezuo: starting WebSocket connection")
		if err := p.wsClient.Start(context.Background()); err != nil {
			slog.Error("xiezuo: WebSocket client error", "error", err)
		}
	}()

	return nil
}

// extractTextContent extracts plain text from the message content.
// For text messages, content is typically: {"text": {"content": "...", "type": "plain"}}
func extractTextContent(content any) string {
	m, ok := content.(map[string]any)
	if !ok {
		return ""
	}
	textObj, ok := m["text"]
	if !ok {
		return ""
	}
	textMap, ok := textObj.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := textMap["content"].(string)
	return s
}

func (p *Platform) Reply(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("xiezuo: invalid reply context type %T", rctx)
	}
	if content == "" {
		return nil
	}
	content = core.StripMarkdown(content)

	// Try reply first, fall back to send
	err := p.replyMessage(ctx, rc.chatID, rc.messageID, content)
	if err != nil {
		slog.Warn("xiezuo: reply failed, falling back to send", "error", err)
		return p.sendMessage(ctx, rc.chatID, content)
	}
	return nil
}

func (p *Platform) Send(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("xiezuo: invalid reply context type %T", rctx)
	}
	if content == "" {
		return nil
	}
	content = core.StripMarkdown(content)
	return p.sendMessage(ctx, rc.chatID, content)
}

func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	// xiezuo:{chatID}
	parts := strings.SplitN(sessionKey, ":", 2)
	if len(parts) < 2 || parts[0] != "xiezuo" {
		return nil, fmt.Errorf("xiezuo: invalid session key %q", sessionKey)
	}
	return replyContext{chatID: parts[1]}, nil
}

func (p *Platform) Stop() error {
	if p.wsClient != nil {
		return p.wsClient.Stop()
	}
	return nil
}

// ==================== WPS API ====================

type textBody struct {
	Content string `json:"content"`
	Type    string `json:"type,omitempty"`
}

type messageContent struct {
	Text *textBody `json:"text,omitempty"`
}

type receiver struct {
	ReceiverID string `json:"receiver_id"`
	Type       string `json:"type"`
}

type sendMessageRequest struct {
	Type     string         `json:"type"`
	Receiver receiver       `json:"receiver"`
	Content  messageContent `json:"content"`
}

type replyMessageRequest struct {
	Type    string         `json:"type"`
	Content messageContent `json:"content"`
}

type apiResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg,omitempty"`
}

func (p *Platform) sendMessage(ctx context.Context, chatID, text string) error {
	req := &sendMessageRequest{
		Type:     "text",
		Receiver: receiver{ReceiverID: chatID, Type: "chat"},
		Content:  messageContent{Text: &textBody{Content: text, Type: "plain"}},
	}
	resp, err := p.post(ctx, "/v7/messages/create", req)
	if err != nil {
		return fmt.Errorf("xiezuo: send message: %w", err)
	}
	if resp.Code != 0 {
		return fmt.Errorf("xiezuo: send message: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (p *Platform) replyMessage(ctx context.Context, chatID, messageID, text string) error {
	req := &replyMessageRequest{
		Type:    "text",
		Content: messageContent{Text: &textBody{Content: text, Type: "plain"}},
	}
	path := fmt.Sprintf("/v7/chats/%s/messages/%s/reply", chatID, messageID)
	resp, err := p.post(ctx, path, req)
	if err != nil {
		return fmt.Errorf("xiezuo: reply message: %w", err)
	}
	if resp.Code != 0 {
		return fmt.Errorf("xiezuo: reply message: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// ==================== HTTP + Auth ====================

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	Code        int    `json:"code"`
	Msg         string `json:"msg"`
}

func (p *Platform) getAccessToken(ctx context.Context) (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()

	if p.cachedToken != "" && time.Until(p.tokenExpiry) > 5*time.Minute {
		return p.cachedToken, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", p.appID)
	form.Set("client_secret", p.appKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBaseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("xiezuo: create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := core.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("xiezuo: fetch token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("xiezuo: read token response: %w", err)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("xiezuo: unmarshal token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("xiezuo: get access_token failed: code=%d msg=%s", tr.Code, tr.Msg)
	}

	p.cachedToken = tr.AccessToken
	p.tokenExpiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return p.cachedToken, nil
}

func (p *Platform) post(ctx context.Context, path string, payload any) (*apiResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	date := time.Now().UTC().Format(http.TimeFormat)
	authorization := p.sign(http.MethodPost, path, contentType, body, date)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	accessToken, err := p.getAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Kso-Date", date)
	req.Header.Set("X-Kso-Authorization", authorization)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := core.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	var result apiResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response (status=%d): %w", resp.StatusCode, err)
	}
	return &result, nil
}

// sign generates the KSO-1 Authorization header value.
func (p *Platform) sign(method, uri, ct string, body []byte, date string) string {
	sha256Hex := ""
	if len(body) > 0 {
		h := sha256.New()
		h.Write(body)
		sha256Hex = hex.EncodeToString(h.Sum(nil))
	}

	stringToSign := kso1Type + method + uri + ct + date + sha256Hex

	mac := hmac.New(sha256.New, []byte(p.appKey))
	mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	return fmt.Sprintf("%s %s:%s", kso1Type, p.appID, signature)
}
