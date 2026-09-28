package daemon

import (
	"reflect"
	"testing"

	"github.com/ehsan200/em-xray/core/xray"
)

func TestMovedMasters(t *testing.T) {
	old := []xray.Slot{
		{Master: "a", Aliases: []string{"b"}, Key: "xraysub:s1"},
		{Master: "c", Key: "xray:e"},
		{Master: "d", Key: "xraysub:s2"},
	}
	cur := []xray.Slot{
		{Master: "a", Key: "xraysub:s1"},                         // a keeps its pool
		{Master: "b", Aliases: []string{"c"}, Key: "xraysub:s2"}, // b and c move
		{Master: "n", Key: "xray:e"},                             // n is new
		// d is no longer a master
	}
	if got, want := movedMasters(old, cur), []string{"b", "c", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("moved = %v, want %v", got, want)
	}
	if got := movedMasters(nil, cur); got != nil {
		t.Fatalf("nothing was running, moved = %v", got)
	}
	if got := movedMasters(cur, cur); got != nil {
		t.Fatalf("no change, moved = %v", got)
	}
}
