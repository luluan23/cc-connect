# client.go
```go
// Package wpsapi 提供 WPS365 开放平台 HTTP API 调用能力
package wpsapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	baseURL     = "https://openapi.wps.cn"
	kso1Type    = "KSO-1"
	contentType = "application/json"
)

// Client WPS365 开放平台 API 客户端
type Client struct {
	appID     string
	appSecret string
	http      *http.Client

	// access token 缓存（有效期 2 小时，提前 5 分钟刷新）
	tokenMu     sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

// NewClient 创建 API 客户端
//   - appID:     应用 ID
//   - appSecret: 应用密钥（用于 KSO-1 签名 及 OAuth2 获取 token）
func NewClient(appID, appSecret string) *Client {
	return &Client{
		appID:     appID,
		appSecret: appSecret,
		http: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// ==================== OAuth2 ====================

// tokenResponse OAuth2 token 接口响应
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	// 失败时
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// getAccessToken 获取有效的 access_token（优先使用缓存，过期前 5 分钟自动刷新）
func (c *Client) getAccessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// 距过期还剩超过 5 分钟时直接复用
	if c.cachedToken != "" && time.Until(c.tokenExpiry) > 5*time.Minute {
		return c.cachedToken, nil
	}

	// 重新获取
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.appID)
	form.Set("client_secret", c.appSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("unmarshal token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("get access_token failed: code=%d msg=%s", tr.Code, tr.Msg)
	}

	c.cachedToken = tr.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.cachedToken, nil
}

// ==================== 消息内容（content）结构 ====================

// TextBody 文本消息内容（type=text 时 content.text 的值）
type TextBody struct {
	// Content 文本内容，支持 <at id="0"> 张三 </at> 语法
	Content string `json:"content"`
	// Type 文本类型：plain（纯文本）/ markdown；可选
	Type string `json:"type,omitempty"`
}

// ImageBody 图片消息内容（type=image 时 content.image 的值）
type ImageBody struct {
	StorageKey          string `json:"storage_key"`
	Type                string `json:"type,omitempty"`
	ThumbnailStorageKey string `json:"thumbnail_storage_key,omitempty"`
	ThumbnailType       string `json:"thumbnail_type,omitempty"`
	Name                string `json:"name,omitempty"`
	Size                int    `json:"size,omitempty"`
	Width               int    `json:"width,omitempty"`
	Height              int    `json:"height,omitempty"`
}

// MessageContent 消息内容，根据消息类型（type）填充对应字段：
//   - type=text      → 填 Text
//   - type=rich_text → 填 RichText（自由构造 map）
//   - type=image     → 填 Image
//
// 其他类型（file/audio/video/card）可直接使用 json.RawMessage 扩展。
type MessageContent struct {
	Text     *TextBody      `json:"text,omitempty"`
	RichText map[string]any `json:"rich_text,omitempty"`
	Image    *ImageBody     `json:"image,omitempty"`
}

// ==================== 消息 API ====================
// Receiver 消息接收者
type Receiver struct {
	// ReceiverID 接收者 id
	ReceiverID string `json:"receiver_id"`
	// Type 接收者类型：user（企业成员）/ enterprise_partner_user（关联组织成员）/ chat（会话）
	Type string `json:"type"`
	// PartnerID 关联组织 id，接收者为关联组织成员时填写
	PartnerID string `json:"partner_id,omitempty"`
}

// MentionIdentity 被 @ 用户身份
type MentionIdentity struct {
	CompanyID string `json:"company_id"`
	ID        string `json:"id"`
	Type      string `json:"type"` // user
}

// Mention 消息 @ 信息
type Mention struct {
	// ID 与消息正文 <at id={index}> 中的 {index} 匹配
	ID string `json:"id"`
	// Type at 对象类型：all（所有人）/ user
	Type string `json:"type"`
	// Identity 被 @ 的用户信息，at 所有人时为空
	Identity *MentionIdentity `json:"identity,omitempty"`
}

// SendMessageRequest 发送消息请求体
// POST https://openapi.wps.cn/v7/messages/create
type SendMessageRequest struct {
	// Type 消息类型：text / rich_text / image / file / audio / video / card
	Type string `json:"type"`
	// Receiver 消息接收者
	Receiver Receiver `json:"receiver"`
	// Content 消息内容（根据 Type 传不同结构）
	Content MessageContent `json:"content"`
	// Mentions 被 @ 人员列表（可选，最多 100 个）
	Mentions []Mention `json:"mentions,omitempty"`
}

// ReplyMessageRequest 回复消息请求体
// POST https://openapi.wps.cn/v7/chats/{chat_id}/messages/{message_id}/reply
type ReplyMessageRequest struct {
	// Type 消息类型：text / rich_text / image
	Type string `json:"type"`
	// Content 消息内容（根据 Type 传不同结构）
	Content MessageContent `json:"content"`
	// Mentions 被 @ 人员列表（可选，最多 100 个）
	Mentions []Mention `json:"mentions,omitempty"`
}

// MessageResponse API 通用响应结构
type MessageResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// SendMessage 发送消息到指定会话
func (c *Client) SendMessage(ctx context.Context, req *SendMessageRequest) (*MessageResponse, error) {
	path := "/v7/messages/create"
	return c.post(ctx, path, req)
}

// ReplyMessage 回复指定消息
func (c *Client) ReplyMessage(ctx context.Context, chatID, messageID string, req *ReplyMessageRequest) (*MessageResponse, error) {
	path := fmt.Sprintf("/v7/chats/%s/messages/%s/reply", chatID, messageID)
	return c.post(ctx, path, req)
}

// ==================== 内部方法 ====================

// post 发起带 KSO-1 签名的 POST 请求
func (c *Client) post(ctx context.Context, path string, payload any) (*MessageResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	date := time.Now().UTC().Format(http.TimeFormat)
	authorization, err := c.sign(http.MethodPost, path, contentType, body, date)
	if err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	accessToken, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Kso-Date", date)
	req.Header.Set("X-Kso-Authorization", authorization)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	var result MessageResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response (status=%d body=%s): %w", resp.StatusCode, string(respBody), err)
	}

	return &result, nil
}

// sign 按 KSO-1 算法生成 Authorization 头值
//
// 签名字符串 = "KSO-1" + method + uri + contentType + date + sha256hex(body)
func (c *Client) sign(method, uri, ct string, body []byte, date string) (string, error) {
	sha256Hex := ""
	if len(body) > 0 {
		h := sha256.New()
		h.Write(body)
		sha256Hex = hex.EncodeToString(h.Sum(nil))
	}

	stringToSign := kso1Type + method + uri + ct + date + sha256Hex

	mac := hmac.New(sha256.New, []byte(c.appSecret))
	mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	return fmt.Sprintf("%s %s:%s", kso1Type, c.appID, signature), nil
}

```

# main.go
```go
package main

import (
	"context"
	"log"
	"os"

	openevent "github.com/GongchuangSu/open-event-sdk-go"

	"wps-xiezuo-robot/wpsapi"
)

// 环境变量Name
const (
	ENV_APP_ID  = "XIEZUO_APP_ID"
	ENV_APP_KEY = "XIEZUO_APP_KEY"
)

var (
	appID  = os.Getenv(ENV_APP_ID)
	appKey = os.Getenv(ENV_APP_KEY)
)

func main() {
	// 创建 WPS365 API 客户端（自动获取并缓存 access_token）
	api := wpsapi.NewClient(appID, appKey)

	// 创建事件分发器
	dispatcher := openevent.NewDispatcher()

	// 处理用户发给机器人的消息事件（单聊 / 群聊）
	dispatcher.OnV7AppChatMessageCreate(func(ctx context.Context, e *openevent.V7AppChatMessageCreateEvent) error {
		chatID := e.Data.Chat.Id
		messageID := e.Data.Message.Id
		chatType := e.Data.Chat.Type
		senderID := e.Data.Sender.Id

		log.Printf("收到消息: chat_id=%s, message_id=%s, chat_type=%s, sender=%s",
			chatID, messageID, chatType, senderID)

		replyText := "你好！我已收到你的消息，稍后为你处理。"

		// 优先使用【回复消息】接口（保留消息引用上下文）
		replyResp, err := api.ReplyMessage(ctx, chatID, messageID, &wpsapi.ReplyMessageRequest{
			Type:    "text",
			Content: wpsapi.MessageContent{Text: &wpsapi.TextBody{Content: replyText, Type: "plain"}},
		})
		if err != nil || replyResp.Code != 0 {
			log.Printf("回复消息失败，尝试直接发送: %v", err)
			// 降级：使用「发送消息」接口
			sendResp, sendErr := api.SendMessage(ctx, &wpsapi.SendMessageRequest{
				Type: "text",
				Receiver: wpsapi.Receiver{
					ReceiverID: chatID,
					Type:       "chat",
				},
				Content: wpsapi.MessageContent{Text: &wpsapi.TextBody{Content: replyText, Type: "plain"}},
			})
			if sendErr != nil {
				return sendErr
			}
			log.Printf("发送消息成功: code=%d", sendResp.Code)
			return nil
		}

		log.Printf("回复消息成功: code=%d", replyResp.Code)
		return nil
	})

	// 处理首次创建会话事件（用户首次和机器人聊天）
	dispatcher.OnV7AppChatCreate(func(ctx context.Context, e *openevent.V7AppChatCreateEvent) error {
		chatID := e.Data.ChatId
		log.Printf("新会话创建: chat_id=%s", chatID)

		// 主动发送欢迎消息
		resp, err := api.SendMessage(ctx, &wpsapi.SendMessageRequest{
			Type: "text",
			Receiver: wpsapi.Receiver{
				ReceiverID: chatID,
				Type:       "chat",
			},
			Content: wpsapi.MessageContent{Text: &wpsapi.TextBody{Content: "你好！我是写作机器人，有什么可以帮你的？"}},
		})
		if err != nil {
			return err
		}
		log.Printf("发送欢迎消息成功: code=%d", resp.Code)
		return nil
	})

	// 注册兜底处理器
	dispatcher.RegisterFallbackFunc(func(ctx context.Context, e *openevent.Event) error {
		log.Printf("未处理事件: event_code=%s", e.EventCode())
		return nil
	})

	// 创建 WebSocket 长连接客户端
	wsClient := openevent.NewClient(appID, appKey,
		openevent.WithDispatcher(dispatcher),
	)

	if err := wsClient.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
}

```
