package anthropic

import (
	"fmt"
	"io"
	"strconv"
)

// String describes the adapter by its model id. It never includes the API key (or the base URL,
// which can carry credentials of its own), so a config that holds a *Model can be logged.
func (m Model) String() string {
	return "anthropic.Model{model:" + strconv.Quote(m.model) + ", apiKey:[redacted]}"
}

// Format writes String for every verb, so no fmt verb (%v, %+v, %#v, %s, %x, ...) prints the key.
func (m Model) Format(f fmt.State, _ rune) { io.WriteString(f, m.String()) }
