package saga

import (
	"testing"

	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// The topic names are the connector's, not this package's invention: getting
// them wrong means subscribing to topics nobody publishes to, which looks
// exactly like an idle system.
func TestTopic_MatchesThePublishedNames(t *testing.T) {
	t.Parallel()

	for aggregateType, want := range map[string]string{
		transfer.AggregateType:    "transfer-events",
		transaction.AggregateType: "transaction-events",
	} {
		if got := Topic(aggregateType); got != want {
			t.Errorf("Topic(%q) = %q, want %q", aggregateType, got, want)
		}
	}
}
