package telegram

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"touchgocore/config"
	"touchgocore/localtimer"
	"touchgocore/util"
	"touchgocore/vars"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func init() {
	util.DefaultCallFunc.Register(util.CallTelegramMsg+"StartMessage", startMessage)
}

func startMessage(bot *tgbotapi.BotAPI, chat_id int64, desc, bannelurl string) error {
	// 构建游戏链接
	vars.Info("telegram start game link", config.Cfg_.Telegram.GameToShort)

	photo := tgbotapi.NewPhoto(
		chat_id,
		tgbotapi.FileURL(config.Cfg_.Telegram.GameBannerUrl),
	)

	if desc == "" { //开始消息
		photo.Caption = config.Cfg_.Telegram.GameDescription
		photo.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{},
		}

		pt1 := photo.ReplyMarkup.(*tgbotapi.InlineKeyboardMarkup)
		InlineKeyboard := &pt1.InlineKeyboard
		linemax := 2 //没排按钮最多2个
		cnt := 0
		idx := 0
		for key, gameurl := range config.Cfg_.Telegram.GameToShort {
			if cnt%linemax == 0 {
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
		if bannelurl != "" {
			photo.File = tgbotapi.FileURL(bannelurl)
		}
		photo.Caption = desc
	}

	// 发送消息
	if _, err := bot.Send(photo); err != nil {
		vars.Error("telegram send message error", err)
		return err
	}
	return nil
}

// 处理文本消息
func handleMessage(bot *tgbotapi.BotAPI, message *tgbotapi.Message) {
	// 处理/start命令
	if message.Text == "/start" {
		startMessage(bot, message.Chat.ID, "", "")
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
		if message.Text[0] == '/' {
			util.DefaultCallFunc.Do(util.CallTelegramMsg+message.Text, bot, message)
		} else {
			//说话消息
			util.DefaultCallFunc.Do(util.CallTelegramMsg+"Say", message.Text, bot, message)
		}
	}
}

// 处理回调查询（游戏跳转确认）
func handleCallback(bot *tgbotapi.BotAPI, callback *tgbotapi.CallbackQuery) {
	if callback.GameShortName != "" {
		// 构建回调响应
		answer := tgbotapi.NewCallback(callback.ID, "")
		answer.ShowAlert = false
		answer.URL = config.Cfg_.Telegram.GameToShort[callback.GameShortName] // 关键字段：触发Mini App跳转

		// 发送确认响应
		_, err := bot.Send(answer)
		if err != nil {
			vars.Error("回调响应失败", err)
			return
		}
	}
}

var closeCh chan any

type telegramTimer struct {
	localtimer.Timer
	bot *tgbotapi.BotAPI
}

func (t *telegramTimer) Tick() {
	//每分钟广播一次心跳
	util.DefaultCallFunc.Do(util.CallTelegramMsg+"Minute", t.bot)
}

// 机器人监听代码
func TelegramStart() {
	if config.Cfg_.Telegram == nil || config.Cfg_.Telegram.BotToken == "" {
		return
	}

	bot, err := tgbotapi.NewBotAPI(config.Cfg_.Telegram.BotToken)
	if err != nil {
		vars.Error("telegram bot api error", err)
		return
	}

	bot.Debug = true
	vars.Info("Authorized on account", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)
	closeCh = make(chan any)

	//创建定时器,每1分钟发送一次心跳
	t, err := localtimer.NewTimer(60*1000, -1, &telegramTimer{})
	if err != nil {
		vars.Error("telegram timer error", err)
		return
	}
	timer := t.(*telegramTimer)
	timer.bot = bot
	localtimer.AddTimer(timer)

	//创建机器人消息监听
	go func() {
		for update := range updates {
			select {
			case _, ok := <-closeCh:
				if !ok {
					timer.Remove() //删除定时器
					return
				}
			default:
				if update.Message != nil {
					handleMessage(bot, update.Message)
				} else if update.CallbackQuery != nil {
					handleCallback(bot, update.CallbackQuery)
				}
			}
		}
	}()
}

func TelegramStop() {
	if config.Cfg_.Telegram == nil || config.Cfg_.Telegram.BotToken == "" {
		return
	}
	close(closeCh)
}

// ValidateWebAppData 验证Telegram WebApp数据
// botToken: 机器人的Token
// data: 原始查询字符串（例如："user=auth_date=...&hash=..."）
// 返回：验证成功后的键值对map，或错误信息
func validateWebAppData(botToken, data string) (map[string]interface{}, error) {
	defer func() {
		if condition := recover(); condition != nil {
			vars.Error("validateWebAppData panic", condition)
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
		dataCheckStr.WriteString(fmt.Sprintf("%s=%s", key, value))
	}

	// 计算密钥
	h := hmac.New(sha256.New, []byte("WebAppData"))
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
			vars.Error("telegram verify panic", condition)
		}
	}()

	if config.Cfg_.Telegram == nil || config.Cfg_.Telegram.BotToken == "" {
		vars.Error("telegram verify error", "telegram config error")
		return "", "", errors.New("telegram config error")
	}

	result, err := validateWebAppData(config.Cfg_.Telegram.BotToken, data)
	if err != nil {
		vars.Error("telegram verify error", err)
		return "", "", err
	}
	vars.Info("telegram verify success", result)
	user := result["user"].(map[string]any)
	id := fmt.Sprintf("%d", int64(user["id"].(float64)))
	var username string
	if n, h := user["username"]; h {
		username = n.(string)
	} else {
		if n1, h := user["first_name"]; h {
			username = n1.(string)
		}
	}
	return id, username, nil
}
