package main

import (
	"fmt"
	"reflect"
	"testing"
)

func TestAddRelayHistoryKeepsRecentUniqueValidAddresses(t *testing.T) {
	history := addRelayHistory([]string{
		"relay.example:42056",
		"RELAY.example:42056",
		"bad host:invalid",
		"192.0.2.2",
	}, "Relay.Example:42056")
	want := []string{"relay.example:42056", "192.0.2.2:42056"}
	if !reflect.DeepEqual(history, want) {
		t.Fatalf("got %#v, want %#v", history, want)
	}
}

func TestAddRelayHistoryMovesAddressToFrontAndCapsLength(t *testing.T) {
	history := make([]string, maxRelayHistory+2)
	for index := range history {
		history[index] = fmt.Sprintf("relay-%d.example:42056", index)
	}
	history = addRelayHistory(history, "relay-1.example:42056")
	if len(history) != maxRelayHistory || history[0] != "relay-1.example:42056" || history[1] != "relay-0.example:42056" {
		t.Fatalf("unexpected recent relay ordering: %#v", history)
	}
}
