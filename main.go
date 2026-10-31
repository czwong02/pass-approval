package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"pass-approval/internal/channel"
	"pass-approval/internal/db"
	"pass-approval/internal/page"
	"pass-approval/internal/rules"
	"pass-approval/internal/store"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", authed(create))
	mux.HandleFunc("GET /v1/requests/{id}", authed(get))
	mux.HandleFunc("POST /v1/requests/{id}/cancel", authed(cancel))
	mux.HandleFunc("POST /v1/link-codes", authed(linkCode))
	mux.HandleFunc("POST /webhooks/telegram", telegramHook)
	mux.HandleFunc("POST /webhooks/discord", discordHook)
	mux.HandleFunc("GET /webhooks/whatsapp", whatsappChallenge)
	mux.HandleFunc("POST /webhooks/whatsapp", whatsappHook)
	mux.HandleFunc("GET /a/{token}", openPage)
	mux.HandleFunc("POST /a/{token}", openPage)
	mux.HandleFunc("GET /internal/sweep", cronSweep)
	mux.HandleFunc("POST /internal/sweep", cronSweep)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func channels() store.Channels {
	return store.Channels{
		"telegram": channel.Telegram{},
		"discord":  channel.Discord{},
		"whatsapp": channel.WhatsApp{},
	}
}

func open(w http.ResponseWriter, r *http.Request) (*pgx.Conn, bool) {
	conn, err := db.Open(r.Context())
	if err != nil {
		http.Error(w, `{"error":"internal"}`, 500)
		return nil, false
	}
	return conn, true
}

func authed(next func(http.ResponseWriter, *http.Request, *pgx.Conn, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, ok := open(w, r)
		if !ok {
			return
		}
		defer conn.Close(r.Context())
		if err := store.Sweep(r.Context(), conn, channels()); err != nil {
			log.Print(err)
			http.Error(w, `{"error":"internal"}`, 500)
			return
		}
		tenant, ok := store.Tenant(r.Context(), conn, r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		next(w, r, conn, tenant)
	}
}

func create(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, tenant string) {
	in, ok := parseCreate(r.Body)
	if !ok {
		http.Error(w, `{"error":"bad request"}`, 400)
		return
	}
	created, err := store.Create(r.Context(), conn, tenant, in, channels())
	if err != nil {
		if err.Error() == "unknown approver" || err.Error() == "bad request" {
			http.Error(w, `{"error":"bad request"}`, 400)
			return
		}
		log.Print(err)
		http.Error(w, `{"error":"internal"}`, 500)
		return
	}
	writeJSON(w, 201, created)
}

func get(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, tenant string) {
	view, ok, err := store.Get(r.Context(), conn, tenant, r.PathValue("id"))
	if err != nil {
		log.Print(err)
		http.Error(w, `{"error":"internal"}`, 500)
		return
	}
	if !ok {
		http.Error(w, `{"error":"not found"}`, 404)
		return
	}
	writeJSON(w, 200, view)
}

func cancel(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, tenant string) {
	view, ok, err := store.Cancel(r.Context(), conn, tenant, r.PathValue("id"), channels())
	if err != nil {
		log.Print(err)
		http.Error(w, `{"error":"internal"}`, 500)
		return
	}
	if !ok {
		http.Error(w, `{"error":"not found"}`, 404)
		return
	}
	_ = store.Deliver(r.Context(), conn, 10)
	writeJSON(w, 200, view)
}

func linkCode(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, tenant string) {
	var body struct {
		User string `json:"user"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || strings.TrimSpace(body.User) == "" {
		http.Error(w, `{"error":"bad request"}`, 400)
		return
	}
	code, ok, err := store.LinkCode(r.Context(), conn, tenant, strings.TrimSpace(body.User))
	if err != nil {
		log.Print(err)
		http.Error(w, `{"error":"internal"}`, 500)
		return
	}
	if !ok {
		http.Error(w, `{"error":"not found"}`, 404)
		return
	}
	writeJSON(w, 200, map[string]string{"code": code})
}

func telegramHook(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	tg := channel.Telegram{}
	if err := tg.Verify(r.Header, raw); err != nil {
		w.WriteHeader(401)
		return
	}
	var body struct {
		Message struct {
			Text string `json:"text"`
			From struct {
				ID int64 `json:"id"`
			} `json:"from"`
		} `json:"message"`
		CallbackQuery struct {
			ID string `json:"id"`
		} `json:"callback_query"`
	}
	_ = json.Unmarshal(raw, &body)
	conn, ok := open(w, r)
	if !ok {
		return
	}
	defer conn.Close(r.Context())
	text := strings.TrimSpace(body.Message.Text)
	if body.Message.From.ID != 0 && strings.HasPrefix(text, "/link ") && len(strings.Fields(text)) == 2 && len(strings.Fields(text)[1]) == 6 {
		linked, err := store.Redeem(r.Context(), conn, "telegram", itoa(body.Message.From.ID), strings.Fields(text)[1])
		if err != nil {
			log.Print(err)
		}
		title, note := "Not linked", "That code is expired or already used."
		if linked {
			title, note = "Linked", "Approval requests will arrive here."
		}
		_, _ = tg.Send(r.Context(), itoa(body.Message.From.ID), channel.Card{Title: title, Body: note})
		w.WriteHeader(200)
		return
	}
	if body.CallbackQuery.ID != "" {
		_ = channel.AnswerCallback(r.Context(), body.CallbackQuery.ID)
	}
	in, ok := tg.Parse(raw)
	if ok {
		_ = store.Sweep(r.Context(), conn, channels())
		_ = store.Decide(r.Context(), conn, struct{ Token, ExternalUserID, Channel, Action string }{in.Token, in.ExternalUserID, "telegram", ""}, channels())
		_ = store.Deliver(r.Context(), conn, 10)
	}
	w.WriteHeader(200)
}

func discordHook(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	dc := channel.Discord{}
	if err := dc.Verify(r.Header, raw); err != nil {
		w.WriteHeader(401)
		return
	}
	var body struct {
		Type int `json:"type"`
		Data struct {
			Name    string `json:"name"`
			Options []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"options"`
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
	_ = json.Unmarshal(raw, &body)
	if body.Type == 1 {
		writeJSON(w, 200, map[string]int{"type": 1})
		return
	}
	if body.Type == 2 && body.Data.Name == "link" {
		code := ""
		for _, o := range body.Data.Options {
			if o.Name == "code" {
				code = o.Value
			}
		}
		user := body.Member.User.ID
		if user == "" {
			user = body.User.ID
		}
		conn, ok := open(w, r)
		if !ok {
			return
		}
		defer conn.Close(r.Context())
		linked, err := store.Redeem(r.Context(), conn, "discord", user, code)
		if err != nil {
			log.Print(err)
		}
		content := "That code is expired or already used."
		if linked {
			content = "Linked. Approval requests will arrive here."
		}
		writeJSON(w, 200, map[string]any{"type": 4, "data": map[string]any{"content": content, "flags": 64}})
		return
	}
	in, ok := dc.Parse(raw)
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(`{"type":6}`))
	if !ok {
		return
	}
	// ponytail: r.Context() is cancelled when this handler returns, so the decision uses a fresh context.
	// If the Vercel Go runtime freezes the process on return, this goroutine does not finish. The upgrade is a decision that fits inside Discord's 3 seconds once Neon is warm.
	go func() {
		ctx := context.Background()
		conn, err := db.Open(ctx)
		if err != nil {
			log.Print(err)
			return
		}
		defer conn.Close(ctx)
		_ = store.Sweep(ctx, conn, channels())
		_ = store.Decide(ctx, conn, struct{ Token, ExternalUserID, Channel, Action string }{in.Token, in.ExternalUserID, "discord", ""}, channels())
		_ = store.Deliver(ctx, conn, 10)
	}()
}

func whatsappChallenge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") == "subscribe" && q.Get("hub.verify_token") == os.Getenv("WHATSAPP_VERIFY_TOKEN") {
		_, _ = w.Write([]byte(q.Get("hub.challenge")))
		return
	}
	w.WriteHeader(403)
}

func whatsappHook(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	wa := channel.WhatsApp{}
	if err := wa.Verify(r.Header, raw); err != nil {
		w.WriteHeader(401)
		return
	}
	msg := channel.FirstMessage(raw)
	conn, ok := open(w, r)
	if !ok {
		return
	}
	defer conn.Close(r.Context())
	if msg != nil && msg.Type == "text" {
		text := strings.TrimSpace(msg.Text.Body)
		parts := strings.Fields(text)
		if msg.From != "" && len(parts) == 2 && parts[0] == "/link" && len(parts[1]) == 6 {
			linked, err := store.Redeem(r.Context(), conn, "whatsapp", msg.From, parts[1])
			if err != nil {
				log.Print(err)
			}
			title, note := "Not linked", "That code is expired or already used."
			if linked {
				title, note = "Linked", "Approval requests will arrive here."
			}
			_, _ = wa.Send(r.Context(), msg.From, channel.Card{Title: title, Body: note})
			w.WriteHeader(200)
			return
		}
	}
	in, ok := wa.Parse(raw)
	if ok {
		_ = store.Sweep(r.Context(), conn, channels())
		_ = store.Decide(r.Context(), conn, struct{ Token, ExternalUserID, Channel, Action string }{in.Token, in.ExternalUserID, "whatsapp", ""}, channels())
		_ = store.Deliver(r.Context(), conn, 10)
	}
	w.WriteHeader(200)
}

func openPage(w http.ResponseWriter, r *http.Request) {
	conn, ok := open(w, r)
	if !ok {
		return
	}
	defer conn.Close(r.Context())
	if err := store.Sweep(r.Context(), conn, channels()); err != nil {
		log.Print(err)
	}
	tok := r.PathValue("token")
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		action := r.FormValue("action")
		if action != "approve" && action != "reject" {
			w.WriteHeader(400)
			_ = page.Write(w, page.View{Gone: true})
			return
		}
		_ = store.Decide(r.Context(), conn, struct{ Token, ExternalUserID, Channel, Action string }{tok, "", "whatsapp", action}, channels())
		_ = store.Deliver(r.Context(), conn, 10)
	}
	req, found, unused, err := store.Preview(r.Context(), conn, tok)
	if err != nil {
		log.Print(err)
		http.Error(w, "internal", 500)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	if !found {
		_ = page.Write(w, page.View{Gone: true})
		return
	}
	_ = page.Write(w, page.View{
		Title: req.Title, Body: req.Body, Fields: req.Fields, Status: req.Status,
		Buttons: req.Status == "pending" && unused && req.ExpiresAt.After(time.Now()),
	})
}

func cronSweep(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("CRON_SECRET")
	got := r.Header.Get("Authorization")
	want := "Bearer " + secret
	if secret == "" || len(got) != len(want) || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		w.WriteHeader(401)
		return
	}
	conn, ok := open(w, r)
	if !ok {
		return
	}
	defer conn.Close(r.Context())
	if err := store.Sweep(r.Context(), conn, channels()); err != nil {
		log.Print(err)
		http.Error(w, `{"error":"internal"}`, 500)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func parseCreate(body io.Reader) (store.CreateInput, bool) {
	var raw struct {
		ExternalRef string          `json:"external_ref"`
		Title       string          `json:"title"`
		Body        string          `json:"body"`
		Fields      []channel.Field `json:"fields"`
		Rule        string          `json:"rule"`
		Approvers   []string        `json:"approvers"`
		Steps       []rules.Step    `json:"steps"`
		CallbackURL string          `json:"callback_url"`
		ExpiresIn   float64         `json:"expires_in"`
	}
	if json.NewDecoder(body).Decode(&raw) != nil {
		return store.CreateInput{}, false
	}
	ref := clipText(raw.ExternalRef, 200)
	title := clipText(raw.Title, 200)
	callback, ok := httpsURL(raw.CallbackURL)
	expires := int(raw.ExpiresIn)
	if ref == "" || title == "" || !ok || expires < 1 || expires > 60*60*24*366 || float64(expires) != raw.ExpiresIn {
		return store.CreateInput{}, false
	}
	return store.CreateInput{
		ExternalRef: ref, Title: title, Body: clipText(raw.Body, 4000),
		Fields: raw.Fields, Rule: raw.Rule, Approvers: raw.Approvers, Steps: raw.Steps,
		CallbackURL: callback, ExpiresIn: expires,
	}, true
}

func clipText(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > max {
		return ""
	}
	return s
}

func httpsURL(s string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", false
	}
	return u.String(), true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }