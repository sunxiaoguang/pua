package schedulerapi

import (
	"encoding/json"
	"testing"
)

func TestRevisionJSONRoundTripPreservesUint64(t *testing.T) {
	for _, value := range []string{"9007199254740992", MaximumRevision} {
		t.Run(value, func(t *testing.T) {
			original, err := ParseRevision(value)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(struct {
				Revision Revision `json:"revision"`
			}{Revision: original})
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Revision Revision `json:"revision"`
			}
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Revision != original || string(data) != `{"revision":"`+value+`"}` {
				t.Fatalf("revision round trip = %q via %s", decoded.Revision, data)
			}
		})
	}
}

func TestRevisionRejectsNoncanonicalJSON(t *testing.T) {
	for _, input := range []string{`0`, `1`, `""`, `"0"`, `"01"`, `"+1"`, `"1.0"`, `"18446744073709551616"`} {
		t.Run(input, func(t *testing.T) {
			var revision Revision
			if err := json.Unmarshal([]byte(input), &revision); err == nil {
				t.Fatalf("revision %s unexpectedly accepted as %q", input, revision)
			}
		})
	}
}
