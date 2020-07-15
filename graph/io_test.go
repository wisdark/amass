package graph

import (
	"testing"

	"github.com/OWASP/Amass/v3/graphdb"
)

func TestIO(t *testing.T) {
	g := NewGraph(graphdb.NewCayleyGraphMemory())

	for _, tt := range graphTest {
		_, err := g.InsertFQDN(tt.FQDN, tt.Source, tt.Tag, tt.EventID)
		if err != nil {
			t.Fatal("Failed to insert FQDN\n")
		}

		t.Run("Testing GetOutput...", func(t *testing.T) {

			got := g.EventOutput(tt.EventID, nil, nil)
			if got != nil {
				t.Errorf("Failed to get output.\nOutput:%v", got)
			}

		})
	}

}
