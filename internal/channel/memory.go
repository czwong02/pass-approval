package channel

import (
	"context"
	"net/http"
)

// Sent is one recorded Send. Store tests read this instead of calling a chat API.
type Sent struct {
	To   string
	Card Card
}

// Memory is the in-memory channel. It must not open a network connection.
type Memory struct {
	Sent  []Sent
	Edits []Card
}

// Send appends the card and returns an id such as "m1", "m2".
func (m *Memory) Send(_ context.Context, to string, card Card) (string, error) {
	m.Sent = append(m.Sent, Sent{To: to, Card: card})
	return "m" + itoa(len(m.Sent)), nil
}

// Edit appends the card to Edits.
func (m *Memory) Edit(_ context.Context, _, _ string, card Card) error {
	m.Edits = append(m.Edits, card)
	return nil
}

// Verify accepts anything. The memory channel has no signature.
func (m *Memory) Verify(http.Header, []byte) error { return nil }

// Parse returns false. Tests call Decide with a token they already hold.
func (m *Memory) Parse([]byte) (Inbound, bool) { return Inbound{}, false }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}