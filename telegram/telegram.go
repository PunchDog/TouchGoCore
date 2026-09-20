package telegram

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/localtimer"
	"touchgocore/util"
	"touchgocore/vars"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	// WebAppDataKey 用于HMAC密钥计算的常量
	WebAppDataKey = "WebAppData"
	// MaxButtonsPerRow 每行最多按钮数
	MaxButtonsPerRow = 2
)

var (
	// globalBot 原子指针：Start/Stop 与 SendMessage 分属不同 goroutine
	globalBot atomic.Pointer[tgbotapi.BotAPI]
	// currentCancel 当前轮询协程的取消函数，由 runTimeMu 保护；nil 表示未在运行
	currentCancel context.CancelFunc
	runTimeMu     sync.Mutex
	telegramWG    sync.WaitGroup
	// 只读快照：TelegramStart 写入，各回调经 runCtx() 读取。
	// 用 atomic.Pointer 而不是 atomic.Value：后者要求具体类型一致，
	// 而 context.Context 的实现类型会随调用方变化（emptyCtx/valueCtx），存第二个即 panic。
	telegramRunCtx atomic.Pointer[context.Context]
)

func init() {
	setRunCtx(context.Background())
	util.DefaultCallFunc.Register(util.CallTelegramMsg+"StartMessage", SendPhotoMessage)
}

func setRunCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c := ctx
	telegramRunCtx.Store(&c)
}

func runCtx() context.Context {
	if p := telegramRunCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

func telegramCfg() *config.TelegramConfig {
	cfg := corectx.CfgFrom(runCtx())
	if cfg == nil {
		return nil
	}
	return cfg.Telegram
}

func SendPhotoMessage(bot *tgbotapi.BotAPI, chatID int64, desc, bannerURL string) error {
	if bot == nil {
		vars.Error("bot未初始化")
		return fmt.Errorf("bot未初始化")
	}

	// 构建游戏链接
	tg := telegramCfg()
	if tg == nil {
		return fmt.Errorf("telegram config is nil")
	}
	vars.Info("telegram start game link: %v", tg.GameToShort)

	photo := tgbotapi.NewPhoto(
		chatID,
		tgbotapi.FileURL(tg.GameBannerUrl),
	)

	if desc == "" { //开始消息
		photo.Caption = tg.GameDescription
		photo.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{},
		}

		pt1 := photo.ReplyMarkup.(*tgbotapi.InlineKeyboardMarkup)
		InlineKeyboard := &pt1.InlineKeyboard
		cnt := 0
		idx := 0
		for key, gameurl := range tg.GameToShort {
			if cnt%MaxButtonsPerRow == 0 {
				*InlineKeyboard = append(*InlineKeyboard, []tgbotapi.InlineKeyboardButton{})
				idx = len(*InlineKeyboard) - 1
			}
			cnt++
			(*InlineKeyboard)[idx] = append((*InlineKeyboard)[idx],
				tgbotapi.InlineKeyboardButton{
					Text: "play " + key,
					URL:  &gameurl,
				})
		}
	} else { //其他消息
		if bannerURL != "" {
			photo.File = tgbotapi.FileURL(bannerURL)
		}
		photo.Caption = desc
	}

	// 发送消息
	if _, err := bot.Send(photo); err != nil {
		vars.Error("telegram send message error: %v", err)
		return err
	}
	return nil
}

// 发送消息
func SendMessage(chatID int64, desc string) error {
	bot := globalBot.Load()
	if bot == nil {
		vars.Error("telegram bot未初始化")
		return fmt.Errorf("telegram bot未初始化")
	}
	msg := tgbotapi.NewMessage(chatID, desc)
	if _, err := bot.Send(msg); err != nil {
		vars.Error("telegram send message error: %v", err)
		return err
	}
	return nil
}

// 处理文本消息
func handleMessage(bot *tgbotapi.BotAPI, message *tgbotapi.Message) {
	// 处理/start命令
	if message.Text == "/start" {
		SendPhotoMessage(bot, message.Chat.ID, "", "")
		// } else if message.Text == "/game" {
		// 	for _, v := range config.Cfg_.Telegram.GameToShort {
		// 		chat := tgbotapi.NewMessage(message.Chat.ID, v)
		// 		// 发送消息
		// 		if _, err := bot.Send(chat); err != nil {
		// 			log.ZError(context.TODO(), "telegram send message error", err)
		// 		}
		// 	}
		// }
	} else {
		//按设定的命令发消息
		if len(message.Text) > 0 && message.Text[0] == '/' {
			if ok := util.DefaultCallFunc.Do(util.CallTelegramMsg+message.Text, bot, message); !ok {
				vars.Debug("no handler registered for command: %s", message.Text)
			}
		} else {
			//说话消息
			if ok := util.DefaultCallFunc.Do(util.CallTelegramMsg+"Say", message.Text, bot, message); !ok {
				vars.Debug("no handler registered for say message")
			}
		}
	}
}

// 处理回调查询（游戏跳转确认）
func handleCallback(bot *tgbotapi.BotAPI, callback *tgbotapi.CallbackQuery) {
	if callback.GameShortName != "" {
		// 检查游戏URL是否存在
		tg := telegramCfg()
		if tg == nil {
			vars.Error("telegram config is nil")
			return
		}
		gameURL, exists := tg.GameToShort[callback.GameShortName]
		if !exists || gameURL == "" {
			vars.Error("game URL not found for short name: %s", callback.GameShortName)
			return
		}

		// 构建回调响应
		answer := tgbotapi.NewCallback(callback.ID, "")
		answer.ShowAlert = false
		answer.URL = gameURL // 关键字段：触发Mini App跳转

		// 发送确认响应
		_, err := bot.Send(answer)
		if err != nil {
			vars.Error("回调响应失败: %v", err)
			return
		}
	}
}

type telegramTimer struct {
	localtimer.Timer
	bot *tgbotapi.BotAPI
}

func (t *telegramTimer) Tick() {
	//每分钟广播一次心跳
	if ok := util.DefaultCallFunc.Do(util.CallTelegramMsg+"Minute", t.bot); !ok {
		vars.Debug("no handler registered for minute tick")
	}
}

// 机器人监听代码
func TelegramStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	setRunCtx(ctx)
	tg := telegramCfg()
	if tg == nil {
		vars.Info("不启动Telegram")
		return
	}
	//其他位置设置了key,就用其他地方设置的替换
	util.DefaultCallFunc.Do(util.CallTelegramMsg+"BotKey", &tg.BotToken)
	if tg.BotToken == "" {
		vars.Info("不启动Telegram")
		return
	}

	bot, err := tgbotapi.NewBotAPI(tg.BotToken)
	if err != nil {
		vars.Error("telegram bot api error: %v", err)
		return
	}

	bot.Debug = util.DEBUG
	vars.Info("Authorized on account: %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	timer, err := localtimer.NewTimer[*telegramTimer](util.MILLISECONDS_OF_MINUTE, -1, func(t *telegramTimer) {
		t.bot = bot
	})
	if err != nil {
		vars.Error("telegram timer error: %v", err)
		return
	}
	// 不中断 bot 启动（收消息主循环仍可工作），但定时器丢失必须留痕
	if err := localtimer.AddTimer(timer); err != nil {
		vars.Error("telegram 维护定时器注册失败: %v", err)
	}
	// 只有真的会起轮询才对外可见，否则失败路径会留下一个没人清理的 bot
	globalBot.Store(bot)

	// 每轮轮询持有自己的取消函数：Stop 只作废当前这一轮，
	// 旧协程不会改盯新一轮的信号而永久泄漏
	loopCtx, cancel := context.WithCancel(ctx)
	runTimeMu.Lock()
	prevCancel := currentCancel
	currentCancel = cancel
	runTimeMu.Unlock()
	if prevCancel != nil {
		prevCancel()
	}

	telegramWG.Add(1)
	go func() {
		defer telegramWG.Done()
		// Remove = 彻底作废并归还对象池。轮询退出后本轮 timer 不再被任何地方引用，
		// 实例确实永久弃用，因此不需要改用 Pause。
		runUpdates(loopCtx, bot, updates, timer.Remove)
	}()
}

// runUpdates 消费本轮 updates，直到本轮 ctx 取消、更新流关闭或父 ctx 结束。
// bot/updates/timer 清理全部按轮传入，不读包级状态，重启后旧轮不会误用新轮的资源。
func runUpdates(ctx context.Context, bot *tgbotapi.BotAPI, updates <-chan tgbotapi.Update, onClose func()) {
	if onClose != nil {
		defer onClose()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			if update.Message != nil {
				handleMessage(bot, update.Message)
			} else if update.CallbackQuery != nil {
				handleCallback(bot, update.CallbackQuery)
			}
		}
	}
}

// TelegramStop 停止当前这一轮轮询并等待协程退出。未启动时为 no-op，可重复调用。
func TelegramStop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	runTimeMu.Lock()
	cancel := currentCancel
	currentCancel = nil
	runTimeMu.Unlock()

	if bot := globalBot.Swap(nil); bot != nil {
		bot.StopReceivingUpdates()
	}
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		telegramWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		vars.Error("等待 Telegram 协程退出超时: %v", ctx.Err())
	}
}

// ValidateWebAppData 验证Telegram WebApp数据
// botToken: 机器人的Token
// data: 原始查询字符串（例如："user=auth_date=...&hash=..."）
// 返回：验证成功后的键值对map，或错误信息
func validateWebAppData(botToken, data string) (map[string]any, error) {
	defer func() {
		if condition := recover(); condition != nil {
			vars.Error("validateWebAppData panic: %v", condition)
		}
	}()
	// 分割查询字符串为键值对
	pairs := strings.Split(data, "&")
	kvPairs := make([][]string, 0, len(pairs))
	var hashValue string

	// 提取hash并移除它
	for _, pair := range pairs {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		if kv[0] == "hash" {
			hashValue = kv[1]
			continue
		}
		kvPairs = append(kvPairs, kv)
	}

	if hashValue == "" {
		return nil, errors.New("hash not found in data")
	}

	// 按键排序
	sort.Slice(kvPairs, func(i, j int) bool {
		return kvPairs[i][0] < kvPairs[j][0]
	})

	// 构建数据检查字符串
	var dataCheckStr strings.Builder
	for i, kv := range kvPairs {
		if i > 0 {
			dataCheckStr.WriteString("\n")
		}
		key := kv[0]
		value, err := url.QueryUnescape(kv[1])
		if err != nil {
			return nil, fmt.Errorf("failed to unescape value: %v", err)
		}
		fmt.Fprintf(&dataCheckStr, "%s=%s", key, value)
	}

	// 计算密钥
	h := hmac.New(sha256.New, []byte(WebAppDataKey))
	h.Write([]byte(botToken))
	key := h.Sum(nil)

	// 计算服务器哈希
	h = hmac.New(sha256.New, key)
	h.Write([]byte(dataCheckStr.String()))
	serverHash := hex.EncodeToString(h.Sum(nil))

	// 比较哈希
	if serverHash != hashValue {
		return nil, errors.New("invalid hash")
	}

	for _, kv := range kvPairs {
		if kv[0] == "auth_date" {
			ts, err := strconv.ParseInt(kv[1], 10, 64)
			if err != nil {
				return nil, errors.New("invalid auth_date")
			}
			if time.Since(time.Unix(ts, 0)) > 5*time.Minute {
				return nil, errors.New("auth_date expired")
			}
		}
	}

	// 构建结果map
	result := make(map[string]any)
	for _, kv := range kvPairs {
		value, err := url.QueryUnescape(kv[1])
		if err != nil {
			return nil, fmt.Errorf("failed to unescape value: %v", err)
		}

		valuemap := make(map[string]any)
		err = json.Unmarshal([]byte(value), &valuemap)
		if err != nil {
			result[kv[0]] = kv[1]
		} else {
			result[kv[0]] = valuemap
		}
	}

	return result, nil
}

// TelegramVerify 验证并返回Telegram WebApp数据
func TelegramVerify(data string) (string, string, error) {
	defer func() {
		if condition := recover(); condition != nil {
			vars.Error("telegram verify panic: %v", condition)
		}
	}()

	tg := telegramCfg()
	if tg == nil || tg.BotToken == "" {
		vars.Error("telegram verify error: %s", "telegram config error")
		return "", "", errors.New("telegram config error")
	}

	result, err := validateWebAppData(tg.BotToken, data)
	if err != nil {
		vars.Error("telegram verify error: %v", err)
		return "", "", err
	}
	vars.Info("telegram verify success: %v", result)

	// 安全类型断言和提取
	var id, username string
	if userMap, ok := result["user"].(map[string]any); ok {
		// 提取用户ID（支持多种数字类型）
		if idVal, ok := userMap["id"]; ok {
			switch v := idVal.(type) {
			case float64:
				id = fmt.Sprintf("%d", int64(v))
			case int64:
				id = fmt.Sprintf("%d", v)
			case int:
				id = fmt.Sprintf("%d", v)
			case float32:
				id = fmt.Sprintf("%d", int64(v))
			case int32:
				id = fmt.Sprintf("%d", v)
			case uint64:
				id = fmt.Sprintf("%d", v)
			default:
				return "", "", fmt.Errorf("unsupported id type: %T", v)
			}
		} else {
			return "", "", errors.New("user id not found")
		}

		// 提取用户名（优先使用username，其次first_name）
		if n, ok := userMap["username"].(string); ok && n != "" {
			username = n
		} else if n1, ok := userMap["first_name"].(string); ok && n1 != "" {
			username = n1
		}
	} else {
		return "", "", errors.New("user data not found or invalid format")
	}

	return id, username, nil
}
