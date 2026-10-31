package channel

import (
	"context"
	"net/http"
)

type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type Action struct {
	Token string
	Label string
	Style string
}

// Card is what a chat renders. LinkToken is the open token used by the WhatsApp fallback page.
type Card struct {
	Title     string
	Body      string
	Fields    []Field
	Actions   []Action
	LinkToken string
}

// Inbound is one tap. Token is the raw token from the button, not the hash.
type Inbound struct {
	ExternalUserID string
	Token          string
}

// Channel is one chat. Verify runs before Parse. Verify must see the raw body bytes.
type Channel interface {
	Send(ctx context.Context, to string, card Card) (messageID string, err error)
	Edit(ctx context.Context, to, messageID string, card Card) error
	Verify(header http.Header, raw []byte) error
	Parse(body []byte) (Inbound, bool)
}

// Text is the plain body of a card: title, blank line, body, then each field as "label: value".
func Text(card Card) string {
	out := card.Title + "\n\n" + card.Body
	for _, field := range card.Fields {
		out += "\n" + field.Label + ": " + field.Value
	}
	return out
}
