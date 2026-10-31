package channel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
)

type WhatsApp struct{}

type WAMessage struct {
	From string `json:"from"`
	Type string `json:"type"`
	Text struct {
		Body string `json:"body"`
	} `json:"text"`
	Interactive struct {
		ButtonReply struct {
			ID string `json:"id"`
		} `json:"button_reply"`
	} `json:"interactive"`
}

func (WhatsApp) Verify(h http.Header, raw []byte) error {
	mac := hmac.New(sha256.New, []byte(os.Getenv("WHATSAPP_APP_SECRET")))
	_, _ = mac.Write(raw)
	want := []byte("sha256=" + hex.EncodeToString(mac.Sum(nil)))
	got := []byte(h.Get("X-Hub-Signature-256"))
	if len(want) != len(got) || subtle.ConstantTimeCompare(want, got) != 1 {
		return errors.New("unauthorized")
	}
	return nil
}

func (WhatsApp) Parse(body []byte) (Inbound, bool) {
	msg := FirstMessage(body)
	if msg == nil || msg.From == "" || msg.Interactive.ButtonReply.ID == "" {
		return Inbound{}, false
	}
	return Inbound{ExternalUserID: msg.From, Token: msg.Interactive.ButtonReply.ID}, true
}

func (w WhatsApp) Send(ctx context.Context, to string, card Card) (string, error) {
	if len(card.Actions) == 0 {
		res, err := graph(ctx, map[string]any{
			"messaging_product": "whatsapp", "to": to, "type": "text",
			"text": map[string]string{"body": clip(Text(card), 1024)},
		})
		if err != nil {
			return "", err
		}
		return firstID(res), nil
	}
	buttons := make([]any, len(card.Actions))
	for i, a := range card.Actions {
		buttons[i] = map[string]any{"type": "reply", "reply": map[string]string{"id": a.Token, "title": a.Label}}
	}
	res, err := graph(ctx, map[string]any{
		"messaging_product": "whatsapp", "to": to, "type": "interactive",
		"interactive": map[string]any{
			"type":   "button",
			"body":   map[string]string{"text": clip(Text(card), 1024)},
			"action": map[string]any{"buttons": buttons},
		},
	})
	if err != nil {
		return "", err
	}
	if res.Error != nil && res.Error.Code == 131047 && card.LinkToken != "" {
		res, err = graph(ctx, template(to, card.LinkToken))
		if err != nil {
			return "", err
		}
		return firstID(res), nil
	}
	if res.Error != nil {
		return "", errors.New("whatsapp send failed")
	}
	return firstID(res), nil
}

// ponytail: the Cloud API cannot edit an interactive message. This sends a follow-up text.
// Outside the 24-hour window that text fails, and the /a/ page is the record.
func (WhatsApp) Edit(ctx context.Context, to, _ string, card Card) error {
	_, err := graph(ctx, map[string]any{
		"messaging_product": "whatsapp", "to": to, "type": "text",
		"text": map[string]string{"body": clip(Text(card), 1024)},
	})
	return err
}

func FirstMessage(body []byte) *WAMessage {
	var root struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Messages []WAMessage `json:"messages"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if json.Unmarshal(body, &root) != nil || len(root.Entry) == 0 || len(root.Entry[0].Changes) == 0 || len(root.Entry[0].Changes[0].Value.Messages) == 0 {
		return nil
	}
	return &root.Entry[0].Changes[0].Value.Messages[0]
}

func template(to, tok string) map[string]any {
	return map[string]any{
		"messaging_product": "whatsapp", "to": to, "type": "template",
		"template": map[string]any{
			"name": os.Getenv("WHATSAPP_TEMPLATE"), "language": map[string]string{"code": "en"},
			"components": []any{map[string]any{
				"type": "button", "sub_type": "url", "index": "0",
				"parameters": []any{map[string]string{"type": "text", "text": tok}},
			}},
		},
	}
}

type graphResult struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func graph(ctx context.Context, body any) (graphResult, error) {
	raw, _ := json.Marshal(body)
	url := "https://graph.facebook.com/v22.0/" + os.Getenv("WHATSAPP_PHONE_NUMBER_ID") + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return graphResult{}, err
	}
	req.Header.Set("authorization", "Bearer "+os.Getenv("WHATSAPP_TOKEN"))
	req.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return graphResult{}, err
	}
	defer res.Body.Close()
	var out graphResult
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out, nil
}

func firstID(res graphResult) string {
	if len(res.Messages) == 0 || res.Messages[0].ID == "" {
		return "sent"
	}
	return res.Messages[0].ID
}
