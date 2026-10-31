package channel

import (
	"context"
	"fmt"
	"net/http"
)

type Sent struct {
	To   string
	Card Card
}

type Memory struct {
	Sent  []Sent
	Edits []Card
}

func (m *Memory) Send(_ context.Context, to string, card Card) (string, error) {
	m.Sent = append(m.Sent, Sent{To: to, Card: card})
	return fmt.Sprintf("m%d", len(m.Sent)), nil
}

func (m *Memory) Edit(_ context.Context, _, _ string, card Card) error {
	m.Edits = append(m.Edits, card)
	return nil
}

func (m *Memory) Verify(http.Header, []byte) error { return nil }

func (m *Memory) Parse([]byte) (Inbound, bool) { return Inbound{}, false }
