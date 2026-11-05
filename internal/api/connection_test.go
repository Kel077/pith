package api

import (
	"encoding/json"
	"testing"
)

func TestConnectionLocalTupleCompatibility(t *testing.T) {
	var legacy Connection
	if err := json.Unmarshal([]byte(`{"id":1,"remote":"192.0.2.20:53"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["local"]; ok {
		t.Fatal("empty application tuple was not omitted")
	}
	if _, ok := fields["outboundLocal"]; ok {
		t.Fatal("empty outbound tuple was not omitted")
	}
	want := legacy
	want.Local, want.OutboundLocal = "[2001:db8::10]:50000", "[2001:db8::20]:50001"
	data, err = json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Connection
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}
