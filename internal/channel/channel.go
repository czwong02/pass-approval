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

type Card struct {
	Title     string
	Body      string
	Fields    []Field
	Actions   []Action
	LinkToken string
}

type Inbound struct {
	ExternalUserID string
	Token          string
}

type Channel interface {
	Send(ctx context.Context, to string, card Card) (messageID string, err error)
	Edit(ctx context.Context, to, messageID string, card Card) error
	Verify(header http.Header, raw []byte) error
	Parse(body []byte) (Inbound, bool)
}

func Text(card Card) string {
	s := card.Title + "\n\n" + card.Body
	for _, f := range card.Fields {
		s += "\n" + f.Label + ": " + f.Value
	}
	return s
}
