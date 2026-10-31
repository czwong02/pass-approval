package channel

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
)

type Telegram struct{}

func (Telegram) Verify(h http.Header, _ []byte) error {
	a := []byte(os.Getenv("TELEGRAM_WEBHOOK_SECRET"))
	b := []byte(h.Get("X-Telegram-Bot-Api-Secret-Token"))
	if len(a) != len(b) || subtle.ConstantTimeCompare(a, b) != 1 {
		return errors.New("unauthorized")
	}
	return nil
}

func (Telegram) Parse(body []byte) (Inbound, bool) {
	var msg struct {
		CallbackQuery struct {
			Data string `json:"data"`
			From struct {
				ID int64 `json:"id"`
			} `json:"from"`
		} `json:"callback_query"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.CallbackQuery.Data == "" || msg.CallbackQuery.From.ID == 0 {
		return Inbound{}, false
	}
	return Inbound{ExternalUserID: strconv.FormatInt(msg.CallbackQuery.From.ID, 10), Token: msg.CallbackQuery.Data}, true
}

func (Telegram) Send(ctx context.Context, to string, card Card) (string, error) {
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := tg(ctx, "sendMessage", map[string]any{
		"chat_id": to, "text": clip(Text(card), 4000), "reply_markup": keyboard(card),
	}, &out); err != nil {
		return "", err
	}
	return strconv.FormatInt(out.Result.MessageID, 10), nil
}

func (Telegram) Edit(ctx context.Context, to, messageID string, card Card) error {
	id, _ := strconv.ParseInt(messageID, 10, 64)
	return tg(ctx, "editMessageText", map[string]any{
		"chat_id": to, "message_id": id, "text": clip(Text(card), 4000), "reply_markup": keyboard(card),
	}, nil)
}

func AnswerCallback(ctx context.Context, id string) error {
	return tg(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id}, nil)
}

func keyboard(card Card) map[string]any {
	if len(card.Actions) == 0 {
		return map[string]any{"inline_keyboard": []any{}}
	}
	row := make([]any, len(card.Actions))
	for i, a := range card.Actions {
		row[i] = map[string]string{"text": a.Label, "callback_data": a.Token}
	}
	return map[string]any{"inline_keyboard": []any{row}}
}

func tg(ctx context.Context, method string, body any, dest any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+os.Getenv("TELEGRAM_BOT_TOKEN")+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err = io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	var wrap struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(raw, &wrap) != nil || !wrap.OK {
		return errors.New("telegram " + method)
	}
	if dest != nil {
		return json.Unmarshal(raw, dest)
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
