package channel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
)

type Discord struct{}

func (Discord) Verify(h http.Header, raw []byte) error {
	pub, err := hex.DecodeString(os.Getenv("DISCORD_PUBLIC_KEY"))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("unauthorized")
	}
	sig, err := hex.DecodeString(h.Get("X-Signature-Ed25519"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("unauthorized")
	}
	msg := append([]byte(h.Get("X-Signature-Timestamp")), raw...)
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("unauthorized")
	}
	return nil
}

func (Discord) Parse(body []byte) (Inbound, bool) {
	var msg struct {
		Type int `json:"type"`
		Data struct {
			CustomID string `json:"custom_id"`
		} `json:"data"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Member struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"member"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.Type != 3 || msg.Data.CustomID == "" {
		return Inbound{}, false
	}
	id := msg.Member.User.ID
	if id == "" {
		id = msg.User.ID
	}
	if id == "" {
		return Inbound{}, false
	}
	return Inbound{ExternalUserID: id, Token: msg.Data.CustomID}, true
}

func (Discord) Send(ctx context.Context, to string, card Card) (string, error) {
	ch, err := dm(ctx, to)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := discord(ctx, http.MethodPost, "/channels/"+ch+"/messages", payload(card), &out); err != nil {
		return "", err
	}
	return ch + ":" + out.ID, nil
}

func (Discord) Edit(ctx context.Context, to, messageID string, card Card) error {
	ch, id, ok := strings.Cut(messageID, ":")
	if !ok {
		var err error
		ch, err = dm(ctx, to)
		if err != nil {
			return err
		}
		id = messageID
	}
	return discord(ctx, http.MethodPatch, "/channels/"+ch+"/messages/"+id, payload(card), nil)
}

func dm(ctx context.Context, userID string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := discord(ctx, http.MethodPost, "/users/@me/channels", map[string]string{"recipient_id": userID}, &out)
	return out.ID, err
}

func payload(card Card) map[string]any {
	body := map[string]any{"content": clip(Text(card), 2000)}
	if len(card.Actions) == 0 {
		body["components"] = []any{}
		return body
	}
	buttons := make([]any, len(card.Actions))
	for i, a := range card.Actions {
		style := 4
		if a.Style == "primary" {
			style = 1
		}
		buttons[i] = map[string]any{"type": 2, "style": style, "label": a.Label, "custom_id": a.Token}
	}
	body["components"] = []any{map[string]any{"type": 1, "components": buttons}}
	return body
}

func discord(ctx context.Context, method, path string, body any, dest any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, "https://discord.com/api/v10"+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bot "+os.Getenv("DISCORD_BOT_TOKEN"))
	req.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New("discord " + method + " " + path)
	}
	if dest == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(dest)
}